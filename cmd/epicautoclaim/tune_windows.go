//go:build windows

package main

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"runtime/debug"
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
//     the most the process ever has in memory at once goes from 20 MB to
//     under 12, of its own from 7.3 to under 5;
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
//     fixed handful per look.
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
	// (see stagedTransport and stagedFeed).
	stages bool
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

// stagedTransport hands the working set back before each wait on the
// network: before the server's name is looked up, before the connection is
// made, when the request has been sent (the secure connection was set up
// just before; the wait for the answer is the longest part of a look), and
// when the first byte of the answer is in. A try that fails ends with it
// too: a pause follows before the next.
type stagedTransport struct{ base http.RoundTripper }

func (s stagedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { trimNow() },
		ConnectStart:         func(string, string) { trimNow() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { trimNow() },
		GotFirstResponseByte: func() { trimNow() },
	}
	res, err := s.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		trimNow()
	}
	return res, err
}

// stagedFeed hands the working set back between reading the list and
// acting on it.
type stagedFeed struct{ daemon.Feed }

func (f stagedFeed) Fetch(ctx context.Context, country, locale string, now time.Time) (feed.Snapshot, error) {
	snap, err := f.Feed.Fetch(ctx, country, locale, now)
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

// client is the HTTP client the list is read with. Without stages it is
// Go's own (nil); with them, Go's own transport with the stages marked.
// (The measuring build puts its marks inside, so that each is made before
// the working set is handed back.)
func (t *tuning) client() *http.Client {
	base := memBase(nil)
	if !t.stages {
		if base == nil {
			return nil
		}
		return &http.Client{Transport: base, Timeout: 30 * time.Second}
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{Transport: stagedTransport{base: base}, Timeout: 30 * time.Second}
}
