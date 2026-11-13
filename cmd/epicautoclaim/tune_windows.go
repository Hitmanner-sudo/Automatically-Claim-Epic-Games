//go:build windows

package main

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
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
// process", on a Windows machine, memory of the process's own at the end
// of a look):
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
//   - how the list is downloaded (connection kept or not, HTTP/2 or 1.1)
//     made no difference, so that is left as Go does it.
//
// What a look costs beyond that is Windows' own: its name lookup and its
// certificate check load their libraries into the process.
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
	// wsCap is a hard limit on the working set outside claim sessions, in
	// bytes (0: none). See win.CapWorkingSet.
	wsCap int64
	// stages: hand the working set back to Windows at the stages of a look
	// and several times a second while one is in progress (see
	// stagedTransport, stagedFeed and lookTrimmer).
	stages bool
	// look is running while a look is in progress (with stages).
	look lookTrimmer
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

// looking is told when a look begins and when it is over.
func (t *tuning) looking(on bool) {
	if !t.stages {
		return
	}
	if on {
		t.look.begin()
	} else {
		t.look.end()
	}
}

func (t *tuning) apply() {
	if t.procs > 0 {
		runtime.GOMAXPROCS(t.procs)
	}
	debug.SetGCPercent(t.gcPercent)
	debug.SetMemoryLimit(t.limit)
	if t.wsCap > 0 {
		_ = win.CapWorkingSet(uintptr(t.wsCap))
	}
}

// duringClaims wraps what carries out the claim sessions so that they run
// at the claim pace of the garbage collector, and the pace of the looks is
// put back afterwards.
func (t *tuning) duringClaims(c daemon.Claimer) daemon.Claimer {
	return pacedClaimer{Claimer: c, percent: t.claimGCPercent, wsCap: t.wsCap, look: &t.look}
}

type pacedClaimer struct {
	daemon.Claimer
	percent int
	wsCap   int64
	look    *lookTrimmer
}

func (p pacedClaimer) ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error) {
	old := debug.SetGCPercent(p.percent)
	defer debug.SetGCPercent(old)
	// A claim is real work against the clock: nothing is handed back
	// under it.
	p.look.paused.Store(true)
	defer p.look.paused.Store(false)
	if p.wsCap > 0 {
		// A claim is real work against the clock: no limit meanwhile.
		_ = win.CapWorkingSet(0)
		defer func() { _ = win.CapWorkingSet(uintptr(p.wsCap)) }()
	}
	return p.Claimer.ClaimAll(ctx, offers)
}

// A look goes through stages that each need a different part of the program
// and of Windows in memory: looking the server's name up and connecting;
// setting up the secure connection, which has Windows check the server's
// certificate; reading and understanding the answer; and deciding what to
// do about it. Measured, they need 6, 10, and 8 MB; left to pile up, 23.
// So at the end of each stage the program hands back to Windows everything
// it holds (as it does when it goes back to waiting): what the next stage
// needs is back at the first touch, and what it does not need is no longer
// counted against the program. Nothing is ever taken away from a stage in
// the middle of its work, so this cannot make a look slow, which a fixed
// upper limit can (measured too: with one below what the secure connection
// needs at once, a look took four seconds and a million and a half page
// faults instead of a fifth of a second).

// stagedTransport hands the working set back when the connection to the
// server stands; when the request has been sent (the secure connection was
// set up in between, and the wait for the answer, the longest part of a
// look, begins); and when the first byte of the answer is in.
type stagedTransport struct{ base http.RoundTripper }

func (s stagedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{
		ConnectDone:          func(string, string, error) { win.TrimWorkingSet() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { win.TrimWorkingSet() },
		GotFirstResponseByte: func() { win.TrimWorkingSet() },
	}
	return s.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}

// lookTrimmer hands the working set back four times a second for as long
// as a look is in progress. The stages above are exact but know only a
// look that goes well; this covers the rest: a name server or a server
// that is slow to answer, the pauses between tries when the list cannot be
// read, and whatever else a look comes to do. Its cost is bounded: each
// time, what is in use at that moment comes back at the first touch (some
// thousand page faults of two or three microseconds). While the program
// waits for the next look it does not run at all, and during a claim
// session it holds still.
type lookTrimmer struct {
	mu     sync.Mutex
	stop   chan struct{}
	paused atomic.Bool
}

const lookTrimEvery = 250 * time.Millisecond

func (l *lookTrimmer) begin() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil {
		return
	}
	stop := make(chan struct{})
	l.stop = stop
	go func() {
		t := time.NewTicker(lookTrimEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !l.paused.Load() {
					win.TrimWorkingSet()
				}
			}
		}
	}()
}

func (l *lookTrimmer) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil {
		close(l.stop)
		l.stop = nil
	}
}

// stagedFeed hands the working set back between reading the list and
// acting on it.
type stagedFeed struct{ daemon.Feed }

func (f stagedFeed) Fetch(ctx context.Context, country, locale string, now time.Time) (feed.Snapshot, error) {
	snap, err := f.Feed.Fetch(ctx, country, locale, now)
	win.TrimWorkingSet()
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
