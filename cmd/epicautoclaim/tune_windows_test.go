//go:build windows

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	// When the connection stood, when the request was sent, and when the
	// first byte of the answer was in.
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

type slowClaimer struct{ during func() }

func (c slowClaimer) ClaimAll(context.Context, []feed.Offer) (map[string]claim.Result, error) {
	c.during()
	return nil, nil
}

func TestTheTrimmerRunsOnlyDuringALookAndHoldsStillDuringAClaim(t *testing.T) {
	n := countTrims(t)
	tune := &tuning{stages: true, claimGCPercent: 25}

	// Not running: nothing is handed back.
	time.Sleep(3 * lookTrimEvery)
	if n.Load() != 0 {
		t.Fatalf("handed back %d time(s) with no look in progress", n.Load())
	}

	// A look: several times a second. Beginning twice starts it once.
	tune.looking(true)
	tune.looking(true)
	time.Sleep(4*lookTrimEvery + lookTrimEvery/2)
	during := n.Load()
	if during < 2 || during > 6 {
		t.Fatalf("handed back %d time(s) in a look of %v, want about 4", during, 4*lookTrimEvery)
	}

	// A claim session inside the look: it holds still, and goes on after.
	_, _ = tune.duringClaims(slowClaimer{during: func() {
		time.Sleep(lookTrimEvery / 2) // (a tick that was under way)
		before := n.Load()
		time.Sleep(4 * lookTrimEvery)
		if got := n.Load(); got != before {
			t.Errorf("handed back %d time(s) during a claim session", got-before)
		}
	}}).ClaimAll(context.Background(), nil)
	after := n.Load()
	time.Sleep(3 * lookTrimEvery)
	if n.Load() == after {
		t.Error("nothing is handed back any more after the claim session")
	}

	// The look is over: it stops. Ending twice is harmless.
	tune.looking(false)
	tune.looking(false)
	time.Sleep(lookTrimEvery / 2)
	stopped := n.Load()
	time.Sleep(3 * lookTrimEvery)
	if n.Load() != stopped {
		t.Fatalf("handed back %d more time(s) after the look was over", n.Load()-stopped)
	}
}
