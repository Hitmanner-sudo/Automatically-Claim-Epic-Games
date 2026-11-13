//go:build windows

package main

import (
	"net/http"
	"runtime"
	"runtime/debug"
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
//   - the garbage collector at its ordinary pace (100): 0.5 MB less than
//     at 25, which made it run twice in the middle of a look whose whole
//     garbage is 1.5 MB. Memory is handed back after every look anyway;
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
}

// daemonTuning is what the background process runs with.
func daemonTuning() tuning {
	t := tuning{procs: 2, gcPercent: 100, limit: 48 << 20}
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
}

// client is the HTTP client the list is read with: Go's own (nil), which
// the measuring build wraps to mark the stages of a download.
func (t tuning) client() *http.Client { return memClient(nil) }
