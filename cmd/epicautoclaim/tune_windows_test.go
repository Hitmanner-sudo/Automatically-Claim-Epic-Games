//go:build windows

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestMemoryIsHandedBackAtTheStagesOfADownload(t *testing.T) {
	n := countTrims(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	c := srv.Client()
	c.Transport = stagedTransport{base: c.Transport}
	res, err := c.Get(srv.URL)
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

type oneFeed struct{ snap feed.Snapshot }

func (f oneFeed) Fetch(context.Context, string, string, time.Time) (feed.Snapshot, error) {
	return f.snap, nil
}

func TestMemoryIsHandedBackAfterTheListIsRead(t *testing.T) {
	n := countTrims(t)
	want := feed.Snapshot{FreeNow: []feed.Offer{{Title: "Game"}}}
	tune := &tuning{stages: true}
	got, err := tune.duringLooks(oneFeed{want}).Fetch(context.Background(), "US", "en-US", time.Now())
	if err != nil || len(got.FreeNow) != 1 || got.FreeNow[0].Title != "Game" {
		t.Fatalf("the list did not come through unchanged: %v, %v", got, err)
	}
	if n.Load() != 1 {
		t.Fatalf("handed back %d time(s) after reading the list, want 1", n.Load())
	}
	// Without stages nothing is wrapped.
	if _, wrapped := (&tuning{}).duringLooks(oneFeed{want}).(stagedFeed); wrapped {
		t.Fatal("the feed is wrapped although stages are off")
	}
}

func TestAFailedTryEndsWithMemoryHandedBack(t *testing.T) {
	n := countTrims(t)
	c := &http.Client{Transport: stagedTransport{base: http.DefaultTransport}, Timeout: 5 * time.Second}
	// Nothing listens there: the try fails, and a pause would follow.
	if res, err := c.Get("http://127.0.0.1:1/"); err == nil {
		res.Body.Close()
		t.Skip("something answers on port 1 of this machine")
	}
	// Before connecting, and when the try had failed.
	if got := n.Load(); got < 2 {
		t.Fatalf("handed back %d time(s) around a failed try, want at least 2", got)
	}
}

type countingClaimer struct{ ran *bool }

func (c countingClaimer) ClaimAll(context.Context, []feed.Offer) (map[string]claim.Result, error) {
	*c.ran = true
	return map[string]claim.Result{"k": {Outcome: claim.Claimed}}, nil
}

func TestAClaimSessionRunsAtItsOwnPaceAndThePaceOfTheLooksComesBack(t *testing.T) {
	was := debug.SetGCPercent(100)
	defer debug.SetGCPercent(was)
	ran := false
	tune := &tuning{claimGCPercent: 25}
	res, err := tune.duringClaims(countingClaimer{&ran}).ClaimAll(context.Background(), nil)
	if err != nil || !ran || res["k"].Outcome != claim.Claimed {
		t.Fatalf("the claim session did not run or its results were lost: %v, %v", res, err)
	}
	if now := debug.SetGCPercent(100); now != 100 {
		t.Fatalf("after a claim session the collector is left at %d, want the pace of the looks (100)", now)
	}
}
