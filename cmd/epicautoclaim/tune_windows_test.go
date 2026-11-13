//go:build windows

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
)

// countTrims replaces the hand-back by a counter for the length of a test.
func countTrims(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	was := trimNow
	trimNow = func() { n.Add(1) }
	t.Cleanup(func() { trimNow = was })
	return &n
}

// gcPercent reads the garbage collector's pace without changing it.
func gcPercent() int {
	p := debug.SetGCPercent(100)
	debug.SetGCPercent(p)
	return p
}

func TestMemoryIsHandedBackAtTheStagesOfADownload(t *testing.T) {
	n := countTrims(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), lookTrace), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "{}" {
		t.Fatalf("the download came through as %q", body)
	}
	// Before connecting, when the request was sent, and when the first
	// byte of the answer was in. (No name to look up: the server is
	// addressed by number.)
	if got := n.Load(); got < 3 {
		t.Fatalf("handed back %d time(s) during a download, want at least 3", got)
	}
}

func TestAFailedTryEndsWithMemoryHandedBack(t *testing.T) {
	n := countTrims(t)
	// (A transport of its own: no proxy from the environment in between.)
	c := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), lookTrace), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing listens there: the try fails, and a pause would follow.
	if res, err := c.Do(req); err == nil {
		res.Body.Close()
		t.Skip("something answers on port 1 of this machine")
	}
	// Before connecting, and when connecting had failed.
	if got := n.Load(); got < 2 {
		t.Fatalf("handed back %d time(s) around a failed try, want at least 2", got)
	}
}

// tracedFeed notes whether the look it is asked for carries the trace.
type tracedFeed struct {
	snap   feed.Snapshot
	traced *bool
}

func (f tracedFeed) Fetch(ctx context.Context, _, _ string, _ time.Time) (feed.Snapshot, error) {
	*f.traced = httptrace.ContextClientTrace(ctx) != nil
	return f.snap, nil
}

func TestALookCarriesTheStagesAndEndsWithMemoryHandedBack(t *testing.T) {
	n := countTrims(t)
	want := feed.Snapshot{FreeNow: []feed.Offer{{Title: "Game"}}}
	traced := false
	tune := &tuning{stages: true}
	got, err := tune.duringLooks(tracedFeed{want, &traced}).Fetch(context.Background(), "US", "en-US", time.Now())
	if err != nil || len(got.FreeNow) != 1 || got.FreeNow[0].Title != "Game" {
		t.Fatalf("the list did not come through unchanged: %v, %v", got, err)
	}
	if !traced {
		t.Fatal("the look was made without the stages on its context")
	}
	if n.Load() != 1 {
		t.Fatalf("handed back %d time(s) after reading the list, want 1", n.Load())
	}
	// Without stages nothing is wrapped.
	traced = false
	if _, err := (&tuning{}).duringLooks(tracedFeed{want, &traced}).Fetch(context.Background(), "US", "en-US", time.Now()); err != nil || traced {
		t.Fatalf("with stages off the look still carries them (traced %v, err %v)", traced, err)
	}
}

type countingClaimer struct{ pace *int }

func (c countingClaimer) ClaimAll(context.Context, []feed.Offer) (map[string]claim.Result, error) {
	*c.pace = gcPercent()
	return map[string]claim.Result{"k": {Outcome: claim.Claimed}}, nil
}

func TestAClaimSessionRunsAtItsOwnPaceAndThePaceOfTheLooksComesBack(t *testing.T) {
	was := debug.SetGCPercent(100)
	defer debug.SetGCPercent(was)
	during := 0
	tune := &tuning{gcPercent: 100, claimGCPercent: 25}
	res, err := tune.duringClaims(countingClaimer{&during}).ClaimAll(context.Background(), nil)
	if err != nil || res["k"].Outcome != claim.Claimed {
		t.Fatalf("the claim session did not run or its results were lost: %v, %v", res, err)
	}
	if during != 25 {
		t.Fatalf("the claim session ran with the collector at %d, want 25", during)
	}
	if now := gcPercent(); now != 100 {
		t.Fatalf("after a claim session the collector is left at %d, want the pace of the looks (100)", now)
	}
}

func TestNothingIsCollectedWhileTheProgramWaits(t *testing.T) {
	was := debug.SetGCPercent(100)
	defer debug.SetGCPercent(was)
	n := countTrims(t)
	delays := restTrimAfter
	restTrimAfter = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond}
	defer func() { restTrimAfter = delays }()
	tune := &tuning{gcPercent: 100, claimGCPercent: 25, stages: true}

	// Going to wait: the collector is off, and what the program still
	// touches while it settles down is handed back a little later.
	tune.resting(true)
	if got := gcPercent(); got != -1 {
		t.Fatalf("while waiting the collector is at %d, want off (-1)", got)
	}
	time.Sleep(400 * time.Millisecond)
	if got := n.Load(); got != 2 {
		t.Fatalf("handed back %d time(s) into the wait, want 2", got)
	}

	// Waking for a look: on again, at the pace of the looks.
	tune.resting(false)
	if got := gcPercent(); got != 100 {
		t.Fatalf("for a look the collector is at %d, want 100", got)
	}

	// A wait that is cut short takes its later hand-backs with it.
	n.Store(0)
	tune.resting(true)
	tune.resting(false)
	time.Sleep(300 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("handed back %d time(s) after the wait was over", got)
	}
	if got := gcPercent(); got != 100 {
		t.Fatalf("after a short wait the collector is at %d, want 100", got)
	}
}
