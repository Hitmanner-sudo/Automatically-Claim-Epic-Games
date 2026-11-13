//go:build windows

package main

import (
	"context"
	"net/http"
	"runtime"
	"runtime/debug"

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
}

// daemonTuning is what the background process runs with.
func daemonTuning() tuning {
	t := tuning{procs: 2, gcPercent: 100, limit: 48 << 20, claimGCPercent: 25}
	if n := runtime.NumCPU(); n < t.procs {
		t.procs = n
	}
	memKnobs(&t) // (does nothing in a normal build)
	return t
}

func (t tuning) apply() {
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
func (t tuning) duringClaims(c daemon.Claimer) daemon.Claimer {
	return pacedClaimer{Claimer: c, percent: t.claimGCPercent, wsCap: t.wsCap}
}

type pacedClaimer struct {
	daemon.Claimer
	percent int
	wsCap   int64
}

func (p pacedClaimer) ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error) {
	old := debug.SetGCPercent(p.percent)
	defer debug.SetGCPercent(old)
	if p.wsCap > 0 {
		// A claim is real work against the clock: no limit meanwhile.
		_ = win.CapWorkingSet(0)
		defer func() { _ = win.CapWorkingSet(uintptr(p.wsCap)) }()
	}
	return p.Claimer.ClaimAll(ctx, offers)
}

// client is the HTTP client the list is read with: Go's own (nil), which
// the measuring build wraps to mark the stages of a download.
func (t tuning) client() *http.Client { return memClient(nil) }
