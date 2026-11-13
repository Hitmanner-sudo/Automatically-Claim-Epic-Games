//go:build windows

package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/daemon"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

// tuning is how the background process keeps itself small. It has next to
// nothing to compute: once an hour it downloads a list of some tens of
// kilobytes and compares it with what it knows.
//
// The values are measured, not guessed (workflow "Memory of the background
// process", on a Windows machine):
//
//   - two threads of its own instead of one per processor: 1.4 MB less on
//     a PC with 24 processors (the Go runtime otherwise prepares caches
//     and a garbage-collector worker for each);
//   - the garbage collector at its ordinary pace (100) for the looks:
//     0.5 MB less than at 25, which made it run twice in the middle of a
//     look whose whole garbage is 1.5 MB. Memory is handed back after
//     every look anyway. A claim is another matter: it handles pages of
//     megabytes, and at 25 it holds about 2 MB less (up to 6 MB less when
//     a failed claim saves its page). So claims run at 25, as they did
//     before any of this was measured;
//   - handing the working set back at the stages of a look (see below):
//     the most the process has in memory at once during a look goes from
//     20 MB to under 12, of its own from 7.3 to under 5;
//   - no garbage collection while the program waits (see resting): the Go
//     runtime otherwise runs one every two minutes whether or not there is
//     anything to collect, and each brings some 2 MB of the program's own
//     back into memory, where they stay until the next look;
//   - how the list is downloaded (connection kept or not, HTTP/2 or 1.1)
//     made no difference, so that is left as Go does it.
//
// What a look still costs is the secure connection: for the tenth of a
// second it takes, Go's cryptography and Windows' certificate check need
// about 10 MB at once, 4.5 of them the program's own.
//
// Looked into and left alone:
//
//   - a hard upper limit on the working set (SetProcessWorkingSetSizeEx
//     with QUOTA_LIMITS_HARDWS_MAX_ENABLE). At 12 MB and more it costs
//     nothing, but then it does no better than the stages; below what the
//     certificate check needs at once (about 10 MB here, another figure
//     on another PC) a look took four seconds and a million and a half
//     page faults;
//   - checking the server's certificate against a few roots built into
//     the program before asking Windows: about 2 MB less for that tenth
//     of a second, for a second way of trusting a server that then has to
//     be kept right;
//   - WinHTTP and the like instead of Go's client (more libraries, the
//     same certificate check), and a helper process for each look (the
//     same memory, in a second row of Task Manager);
//   - handing the working set back on a timer during a look (four times a
//     second was tried): no better than the stages on a PC with memory to
//     spare, and on one without, every time is a chance that what was
//     handed back has to be read from the disk again. The stages are a
//     fixed handful per look;
//   - learning the program's folder from the environment instead of from
//     Windows' shell, which loads a dozen libraries of the shell and the
//     window system at start: 3 MB less in memory for the first instant
//     after starting and nothing after it (the stages hand them back),
//     against the folder then depending on the console a command was
//     typed in;
//   - building against the frozen cryptography module (GOFIPS140=v1.0.0)
//     to be rid of a 32 MB buffer in Go 1.26's own that is set aside and
//     never touched: it shows in "commit size" only, occupies nothing,
//     and Go itself is about to stop setting it aside.
type tuning struct {
	// procs is how many threads may run the program's own code at a time
	// (0: as many as the PC has processors).
	procs int
	// gcPercent and limit: see runtime/debug.SetGCPercent and
	// SetMemoryLimit. The limit is a target, not a wall: a claim, which
	// needs more for a moment, is not held up by it.
	gcPercent int
	limit     int64
	// claimGCPercent is the garbage collector's pace while a claim
	// session runs.
	claimGCPercent int
	// stages: hand the working set back to Windows at the stages of a look
	// (see lookTrace and stagedFeed) and as the program settles down to
	// wait (see resting).
	stages bool

	restMu    sync.Mutex
	restTrims []*time.Timer
}

// daemonTuning is what the background process runs with.
func daemonTuning() *tuning {
	t := &tuning{procs: 2, gcPercent: 100, limit: 48 << 20, claimGCPercent: 25, stages: true}
	if n := runtime.NumCPU(); n < t.procs {
		t.procs = n
	}
	memKnobs(t) // (does nothing in a normal build)
	return t
}

func (t *tuning) apply() {
	if t.procs > 0 {
		runtime.GOMAXPROCS(t.procs)
	}
	debug.SetGCPercent(t.gcPercent)
	debug.SetMemoryLimit(t.limit)
}

// duringClaims wraps what carries out the claim sessions so that they run
// at the claim pace of the garbage collector, and the pace of the looks is
// put back afterwards.
func (t *tuning) duringClaims(c daemon.Claimer) daemon.Claimer {
	return pacedClaimer{Claimer: c, percent: t.claimGCPercent}
}

type pacedClaimer struct {
	daemon.Claimer
	percent int
}

func (p pacedClaimer) ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error) {
	old := debug.SetGCPercent(p.percent)
	defer debug.SetGCPercent(old)
	return p.Claimer.ClaimAll(ctx, offers)
}

// trimNow hands the working set back to Windows. (A variable so that a test
// can count the times.)
var trimNow = win.TrimWorkingSet

// A look goes through stages that each need a different part of the program
// and of Windows in memory: looking the server's name up and connecting;
// setting up the secure connection, which has Windows check the server's
// certificate; reading and understanding the answer; and deciding what to
// do about it. Measured, they need 8, 11, and 7 MB; left to pile up, 20.
// So at the end of each stage the program hands back to Windows everything
// it holds (as it does when it goes back to waiting): what the next stage
// needs is back at the first touch, and what it does not need is no longer
// counted against the program. It is a fixed handful of times per look,
// each before the program waits for something, and nothing is ever taken
// away from a stage in the middle of its work: so it cannot make a look
// slow the way a fixed upper limit can (measured too: with one below what
// the secure connection needs at once, a look took four seconds and a
// million and a half page faults instead of a fifth of a second).

// lookTrace hands the working set back before each wait on the network:
// before the server's name is looked up, before the connection is made,
// when the request has been sent (the secure connection was set up just
// before; the wait for the answer is the longest part of a look), and when
// the first byte of the answer is in. A step that fails hands back too: a
// pause follows before the next try.
//
// It rides on the context of the look, so the list is downloaded by the
// very client, with the very transport, that every other command uses.
var lookTrace = &httptrace.ClientTrace{
	DNSStart: func(httptrace.DNSStartInfo) { trimNow() },
	DNSDone: func(i httptrace.DNSDoneInfo) {
		if i.Err != nil {
			trimNow()
		}
	},
	ConnectStart: func(string, string) { trimNow() },
	ConnectDone: func(_, _ string, err error) {
		if err != nil {
			trimNow()
		}
	},
	TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
		if err != nil {
			trimNow()
		}
	},
	WroteRequest:         func(httptrace.WroteRequestInfo) { trimNow() },
	GotFirstResponseByte: func() { trimNow() },
}

// stagedFeed reads the list with lookTrace, and hands the working set back
// once more between reading the list and acting on it.
type stagedFeed struct{ daemon.Feed }

func (f stagedFeed) Fetch(ctx context.Context, country, locale string, now time.Time) (feed.Snapshot, error) {
	snap, err := f.Feed.Fetch(httptrace.WithClientTrace(ctx, lookTrace), country, locale, now)
	trimNow()
	return snap, err
}

// duringLooks wraps what reads the list (see stagedFeed).
func (t *tuning) duringLooks(f daemon.Feed) daemon.Feed {
	if !t.stages {
		return f
	}
	return stagedFeed{f}
}

// resting is told when the program goes back to waiting for the next look
// and when it wakes for one.
//
// While it waits there is nothing to collect, but the Go runtime starts a
// garbage collection every two minutes all the same, and a collection walks
// through the program's data: some 2 MB that were handed back to Windows
// are in memory again two minutes after every look, and stay. So the
// collector is switched off for the wait (the memory limit stays in force)
// and on again for the look.
//
// And what the program still touches while it settles down (the log, the
// connection to Epic's server that Go keeps for a minute and a half and
// then closes) is handed back a few seconds, and again two minutes, into
// the wait.
func (t *tuning) resting(on bool) {
	t.restMu.Lock()
	defer t.restMu.Unlock()
	for _, tm := range t.restTrims {
		tm.Stop()
	}
	t.restTrims = nil
	if !on {
		debug.SetGCPercent(t.gcPercent)
		return
	}
	debug.SetGCPercent(-1)
	if t.stages {
		for _, after := range restTrimAfter {
			t.restTrims = append(t.restTrims, time.AfterFunc(after, func() { trimNow() }))
		}
	}
}

// restTrimAfter is how far into a wait the working set is handed back once
// more (see resting).
var restTrimAfter = []time.Duration{3 * time.Second, 2 * time.Minute}

// client is the HTTP client the list is read with: nil, which is Go's own
// with a time limit (see feed.Client), the same for every command. (Only
// the measuring build brings one, to mark the stages of a download.)
func (t *tuning) client() *http.Client { return memClient() }
