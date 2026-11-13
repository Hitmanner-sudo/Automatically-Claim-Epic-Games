//go:build windows

package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"time"
)

// tuning is how the background process keeps itself small. It has next to
// nothing to compute: once an hour it downloads a list of some tens of
// kilobytes and compares it with what it knows. Everything here trades
// speed it does not need for memory it then does not touch.
type tuning struct {
	// procs is how many threads may run the program's own code at a time.
	// The Go runtime otherwise prepares for as many as the PC has
	// processors (per-processor caches, a garbage-collector worker each),
	// which on a PC with many of them is memory touched for nothing.
	procs int
	// gcPercent and limit: see runtime/debug.SetGCPercent and
	// SetMemoryLimit. The limit is a target, not a wall: a claim, which
	// needs more for a moment, is not held up by it.
	gcPercent int
	limit     int64
	// http: how the list is downloaded. 0: as any Go program would (the
	// connection is kept for a while, HTTP/2 where offered). 1: the
	// connection is closed after each download. 2: that, and plain
	// HTTP/1.1.
	http int
}

// daemonTuning is what the background process runs with.
func daemonTuning() tuning {
	t := tuning{procs: 2, gcPercent: 25, limit: 48 << 20, http: 1}
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

// client is the HTTP client for the hourly look; nil means Go's default.
func (t tuning) client() *http.Client {
	if t.http == 0 {
		return memClient(nil)
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
		// One request an hour: a connection kept open in between would
		// only sit in memory (its buffers, two goroutines, the TLS state).
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
	}
	if t.http >= 2 {
		tr.ForceAttemptHTTP2 = false
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return memClient(&http.Client{Transport: tr, Timeout: 30 * time.Second})
}
