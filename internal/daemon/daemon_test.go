package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/state"
)

type fakeFeed struct {
	snap  feed.Snapshot
	err   error
	calls atomic.Int32
}

func (f *fakeFeed) Fetch(ctx context.Context, country, locale string, now time.Time) (feed.Snapshot, error) {
	f.calls.Add(1)
	return f.snap, f.err
}

type fakeClaimer struct {
	calls   [][]string
	results map[string]claim.Result
	err     error
}

func (c *fakeClaimer) ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error) {
	var titles []string
	out := map[string]claim.Result{}
	for _, o := range offers {
		titles = append(titles, o.Title)
		if r, ok := c.results[o.Key()]; ok {
			out[o.Key()] = r
		}
	}
	c.calls = append(c.calls, titles)
	return out, c.err
}

type rig struct {
	t     *testing.T
	now   time.Time
	feed  *fakeFeed
	claim *fakeClaimer
	d     *Daemon
	logMu sync.Mutex
	logs  []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), feed: &fakeFeed{}, claim: &fakeClaimer{results: map[string]claim.Result{}}}
	r.d = &Daemon{
		Feed: r.feed, Claimer: r.claim, State: st,
		Country: "US", CheckEvery: time.Hour,
		Now:    func() time.Time { return r.now },
		Jitter: func(max time.Duration) time.Duration { return 0 },
		Logf: func(f string, a ...any) {
			r.logMu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(f, a...))
			r.logMu.Unlock()
		},
	}
	return r
}

func (r *rig) offer(id string, ends time.Duration) feed.Offer {
	return feed.Offer{Title: "Game " + id, Namespace: "ns", OfferID: id, DiscountPrice: 0, Start: r.now.Add(-time.Hour), End: r.now.Add(ends)}
}

func (r *rig) tick() time.Duration {
	r.t.Helper()
	return r.d.Tick(context.Background()).Sub(r.now)
}

func (r *rig) entry(id string) state.Entry {
	for key, e := range r.d.State.All() {
		if strings.HasPrefix(key, "ns:"+id+"@") {
			return e
		}
	}
	return state.Entry{}
}

func (r *rig) logged(sub string) bool {
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestClaimsOnceAndThenLeavesItAlone(t *testing.T) {
	r := newRig(t)
	a := r.offer("a", 5*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed, Detail: "order confirmed"}

	// Regular cadence: 90% of the interval when jitter is zero.
	if next := r.tick(); next != 54*time.Minute {
		t.Errorf("next = %v", next)
	}
	if len(r.claim.calls) != 1 || r.entry("a").Status != state.Claimed {
		t.Fatalf("calls=%v entry=%+v", r.claim.calls, r.entry("a"))
	}
	if !r.logged("CLAIMED: Game a") || !r.logged("free now: Game a") {
		t.Errorf("logs = %q", r.logs)
	}

	r.now = r.now.Add(time.Hour)
	r.tick()
	r.tick()
	if len(r.claim.calls) != 1 {
		t.Fatalf("a claimed game was attempted again: %v", r.claim.calls)
	}
}

func TestNothingFreeMeansNoSession(t *testing.T) {
	r := newRig(t)
	r.feed.snap = feed.Snapshot{}
	r.tick()
	if len(r.claim.calls) != 0 {
		t.Fatalf("calls = %v", r.claim.calls)
	}
}

func TestWakesShortlyAfterTheNextGiveawayStarts(t *testing.T) {
	r := newRig(t)
	r.feed.snap = feed.Snapshot{NextChange: r.now.Add(10 * time.Minute)}
	if next := r.tick(); next != 12*time.Minute {
		t.Errorf("next = %v, want 12m (start + 2m grace)", next)
	}
	// A change far in the future does not delay the regular check.
	r.feed.snap = feed.Snapshot{NextChange: r.now.Add(3 * 24 * time.Hour)}
	if next := r.tick(); next != 54*time.Minute {
		t.Errorf("next = %v", next)
	}
}

func TestFailuresBackOff(t *testing.T) {
	r := newRig(t)
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Failed, Detail: "checkout did not load"}

	want := []time.Duration{20 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 12 * time.Hour}
	for i, w := range want {
		next := r.tick()
		// The regular check (54m) may come sooner than the retry; the
		// retry time itself is what must follow the schedule.
		e := r.entry("a")
		if got := e.NextAttempt.Sub(r.now); got != w {
			t.Fatalf("attempt %d: retry in %v, want %v", i+1, got, w)
		}
		if e.Attempts != i+1 {
			t.Fatalf("attempts = %d", e.Attempts)
		}
		if w < 54*time.Minute && next != w {
			t.Fatalf("attempt %d: next tick in %v, want %v", i+1, next, w)
		}
		// A tick before the retry time must not start another session.
		before := len(r.claim.calls)
		r.now = r.now.Add(w - time.Minute)
		r.tick()
		if len(r.claim.calls) != before {
			t.Fatalf("attempt %d: retried too early", i+1)
		}
		r.now = r.now.Add(time.Minute)
	}
}

func TestBusyIsAPostponementNotAnAttempt(t *testing.T) {
	r := newRig(t)
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.err = &BusyError{Reason: "the launcher window is open"}

	for i := 0; i < 5; i++ {
		if next := r.tick(); next != 10*time.Minute {
			t.Fatalf("next = %v", next)
		}
		r.now = r.now.Add(10 * time.Minute)
	}
	if e := r.entry("a"); e.Attempts != 0 || e.Status != state.Pending {
		t.Fatalf("entry = %+v", e)
	}
	// The reason is reported once, not every ten minutes.
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, "the launcher window is open") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("busy reason logged %d times: %q", n, r.logs)
	}
	// When the launcher frees up, the claim goes ahead.
	r.claim.err = nil
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	r.tick()
	if r.entry("a").Status != state.Claimed {
		t.Fatalf("entry = %+v", r.entry("a"))
	}
}

func TestSessionProblems(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wait     time.Duration
		attempts int
	}{
		{"not signed in", claim.ErrNotSignedIn, 6 * time.Hour, 0},
		{"launcher missing", ErrNotInstalled, 12 * time.Hour, 0},
		{"user opened launcher", ErrYielded, 30 * time.Minute, 0},
		{"wrapped sign-in", fmt.Errorf("session: %w", claim.ErrNotSignedIn), 6 * time.Hour, 0},
		{"something else", errors.New("DevTools endpoint did not come up"), 20 * time.Minute, 1},
	}
	for _, c := range cases {
		r := newRig(t)
		a := r.offer("a", 6*24*time.Hour)
		r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
		r.claim.err = c.err
		r.tick()
		e := r.entry("a")
		if got := e.NextAttempt.Sub(r.now); got != c.wait || e.Attempts != c.attempts || e.Status != state.Pending {
			t.Errorf("%s: retry in %v (want %v), attempts %d (want %d), status %s", c.name, got, c.wait, e.Attempts, c.attempts, e.Status)
		}
	}
}

func TestOutcomes(t *testing.T) {
	cases := []struct {
		outcome claim.Outcome
		status  state.Status
		wait    time.Duration
	}{
		{claim.Owned, state.Owned, 0},
		{claim.Unavailable, state.Skipped, 0},
		{claim.NotFree, state.Skipped, 0},
		{claim.Captcha, state.Pending, 6 * time.Hour},
		{claim.NotSignedIn, state.Pending, 6 * time.Hour},
		{claim.DryRun, state.Pending, 24 * time.Hour},
	}
	for _, c := range cases {
		r := newRig(t)
		a := r.offer("a", 6*24*time.Hour)
		r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
		r.claim.results[a.Key()] = claim.Result{Outcome: c.outcome, Detail: "x"}
		r.tick()
		e := r.entry("a")
		if e.Status != c.status {
			t.Errorf("%s: status %s, want %s", c.outcome, e.Status, c.status)
		}
		if c.wait != 0 && e.NextAttempt.Sub(r.now) != c.wait {
			t.Errorf("%s: retry in %v, want %v", c.outcome, e.NextAttempt.Sub(r.now), c.wait)
		}
		// Finished offers are never attempted again.
		r.now = r.now.Add(48 * time.Hour)
		r.tick()
		if c.status.Done() && len(r.claim.calls) != 1 {
			t.Errorf("%s: attempted again after being settled", c.outcome)
		}
	}
}

func TestPaidLookingOfferIsNeverRetried(t *testing.T) {
	r := newRig(t)
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.NotFree, Detail: "the checkout does not show a zero total"}
	r.tick()
	for i := 0; i < 10; i++ {
		r.now = r.now.Add(13 * time.Hour)
		r.tick()
	}
	if len(r.claim.calls) != 1 {
		t.Fatalf("calls = %d", len(r.claim.calls))
	}
	if !r.logged("was NOT ordered") {
		t.Errorf("logs = %q", r.logs)
	}
}

func TestEndingPromotionsAreNotAttempted(t *testing.T) {
	r := newRig(t)
	ending := r.offer("e", 2*time.Minute)
	ok := r.offer("k", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{ending, ok}}
	r.claim.results[ok.Key()] = claim.Result{Outcome: claim.Claimed}
	r.tick()
	if len(r.claim.calls) != 1 || len(r.claim.calls[0]) != 1 || r.claim.calls[0][0] != "Game k" {
		t.Fatalf("calls = %v", r.claim.calls)
	}
	if r.entry("e").Status != state.Pending {
		t.Fatalf("ending=%+v", r.entry("e"))
	}
}

func TestSameGameGivenAwayAgainIsANewGiveaway(t *testing.T) {
	r := newRig(t)
	a := r.offer("a", 5*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Unavailable, Detail: "region"}
	r.tick()
	// A year later the same offer is free again.
	r.now = r.now.Add(365 * 24 * time.Hour)
	again := r.offer("a", 5*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{again}}
	r.claim.results[again.Key()] = claim.Result{Outcome: claim.Claimed}
	r.tick()
	if len(r.claim.calls) != 2 {
		t.Fatalf("the repeat giveaway was not attempted: %v", r.claim.calls)
	}
}

func TestStaleListIsReadAgainSoon(t *testing.T) {
	r := newRig(t)
	r.feed.snap = feed.Snapshot{Stale: true}
	if next := r.tick(); next != 5*time.Minute {
		t.Fatalf("next = %v", next)
	}
}

func TestSoonestEndingOfferGoesFirstAndUnreachedOnesAreRetried(t *testing.T) {
	r := newRig(t)
	late := r.offer("late", 6*24*time.Hour)
	soon := r.offer("soon", 2*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{late, soon}}
	// The session only got to the first offer before being cut short.
	r.claim.results[soon.Key()] = claim.Result{Outcome: claim.Claimed}
	r.claim.err = ErrYielded
	next := r.tick()
	if got := r.claim.calls[0]; len(got) != 2 || got[0] != "Game soon" {
		t.Fatalf("order = %v", got)
	}
	if r.entry("soon").Status != state.Claimed {
		t.Fatalf("soon = %+v", r.entry("soon"))
	}
	if e := r.entry("late"); e.Status != state.Pending || e.NextAttempt.Sub(r.now) != 30*time.Minute || e.Attempts != 0 {
		t.Fatalf("late = %+v", e)
	}
	if next != 30*time.Minute {
		t.Errorf("next = %v", next)
	}
}

func TestFeedErrorsRetrySoonButNotForever(t *testing.T) {
	r := newRig(t)
	r.feed.err = errors.New("no network")
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 6 * time.Minute}
	for i, w := range want {
		if next := r.tick(); next != w {
			t.Fatalf("failure %d: next = %v, want %v", i+1, next, w)
		}
	}
	for i := 0; i < 40; i++ {
		if next := r.tick(); next > time.Hour {
			t.Fatalf("retry interval grew beyond the regular check: %v", next)
		}
	}
	if len(r.claim.calls) != 0 {
		t.Fatal("no session may start without a list")
	}
	// The same error is reported once.
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, "no network") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("feed error logged %d times", n)
	}
	r.feed.err = nil
	if next := r.tick(); next != 54*time.Minute {
		t.Errorf("after recovery next = %v", next)
	}
}

func TestNeverSpinsFasterThanEveryTwoMinutes(t *testing.T) {
	r := newRig(t)
	r.feed.snap = feed.Snapshot{NextChange: r.now.Add(-time.Hour)} // stale
	if next := r.tick(); next < 2*time.Minute {
		t.Fatalf("next = %v", next)
	}
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}, NextChange: r.now.Add(time.Second)}
	r.claim.err = &BusyError{Reason: "x"}
	if next := r.tick(); next < 2*time.Minute {
		t.Fatalf("next = %v", next)
	}
}

func TestSleepUntilNoticesTheClockNotTheTimer(t *testing.T) {
	// The clock already says the time has come (as after standby): return
	// at once, without waiting for any timer.
	start := time.Now()
	if !sleepUntil(context.Background(), start.Add(-time.Second), time.Now) {
		t.Fatal("should report the time has come")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("waited although the time had passed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepUntil(ctx, time.Now().Add(time.Hour), time.Now) {
		t.Fatal("a cancelled wait must report false")
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	r := newRig(t)
	r.d.Now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.d.Run(ctx, 0); close(done) }()
	deadline := time.After(5 * time.Second)
	for r.feed.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("first round never ran")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
