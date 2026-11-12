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
	during  func()             // runs while the "session" is under way
	inspect func([]feed.Offer) // sees the offers as they are passed in
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
	if c.inspect != nil {
		c.inspect(offers)
	}
	if c.during != nil {
		c.during()
	}
	return out, c.err
}

type rig struct {
	t         *testing.T
	statePath string
	now       time.Time
	feed      *fakeFeed
	claim     *fakeClaimer
	d         *Daemon
	logMu     sync.Mutex
	logs      []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, statePath: statePath, now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), feed: &fakeFeed{}, claim: &fakeClaimer{results: map[string]claim.Result{}}}
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
		{"unsupported launcher", fmt.Errorf("%w: preview", ErrUnsupported), 12 * time.Hour, 0},
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
		{claim.NotFree, state.Pending, 6 * time.Hour},
		{claim.Later, state.Pending, 24 * time.Hour},
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

func TestPaidLookingOfferIsLookedAtAgainOnlyRarely(t *testing.T) {
	// The checkout showed a price: nothing was ordered. Epic's pages do
	// misstate things now and then, so it is looked at again, but on the
	// slow schedule (6 h, 12 h, then daily) and never marked as done.
	r := newRig(t)
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.NotFree, Detail: "the checkout does not show a zero total"}
	r.tick()
	if e := r.entry("a"); e.Status != state.Pending || e.NextAttempt.Sub(r.now) != 6*time.Hour {
		t.Fatalf("after the first look: %+v", e)
	}
	if !r.logged("was NOT ordered") {
		t.Errorf("logs = %q", r.logs)
	}
	// Hourly rounds in between change nothing.
	for i := 0; i < 5; i++ {
		r.now = r.now.Add(time.Hour)
		r.tick()
	}
	if len(r.claim.calls) != 1 {
		t.Fatalf("looked again too soon: %d calls", len(r.claim.calls))
	}
	r.now = r.now.Add(time.Hour)
	r.tick()
	if len(r.claim.calls) != 2 {
		t.Fatalf("calls after six hours = %d", len(r.claim.calls))
	}
	if e := r.entry("a"); e.NextAttempt.Sub(r.now) != 12*time.Hour {
		t.Fatalf("second wait = %v", e.NextAttempt.Sub(r.now))
	}
	// Over the following four days: about one look a day.
	for i := 0; i < 4*24; i++ {
		r.now = r.now.Add(time.Hour)
		r.tick()
	}
	if n := len(r.claim.calls); n < 5 || n > 7 {
		t.Fatalf("calls over the week = %d", n)
	}
	// And when the checkout comes round, the claim goes through.
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	r.now = r.now.Add(24 * time.Hour)
	r.tick()
	if e := r.entry("a"); e.Status != state.Claimed {
		t.Fatalf("final status = %s", e.Status)
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
	// Soon at first (a PC that has just woken up may not be online yet),
	// then more slowly.
	want := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}
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
	go func() { r.d.Run(ctx, RunOptions{}); close(done) }()
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

func TestWaitsCountFromTheEndOfALongSession(t *testing.T) {
	// A session can take a long time (the launcher updating itself, slow
	// pages). The pause before the next try starts when it is over.
	r := newRig(t)
	a := r.offer("a", 6*24*time.Hour)
	b := r.offer("b", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a, b}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Failed, Detail: "the checkout did not become ready"}
	r.claim.during = func() { r.now = r.now.Add(25 * time.Minute) }
	next := r.d.Tick(context.Background())
	if wait := next.Sub(r.now); wait != 10*time.Minute {
		t.Fatalf("next round %v after the session ended, want 10m (the offer the session did not reach)", wait)
	}
	if e := r.entry("a"); e.NextAttempt.Sub(r.now) != 20*time.Minute || !e.LastAttempt.Equal(r.now) {
		t.Fatalf("failed offer: next try in %v", e.NextAttempt.Sub(r.now))
	}
	// A busy launcher is looked at again ten minutes after it was found busy.
	r = newRig(t)
	a = r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.err = &BusyError{Reason: "the launcher window is open"}
	r.claim.during = func() { r.now = r.now.Add(time.Minute) }
	if wait := r.d.Tick(context.Background()).Sub(r.now); wait != 10*time.Minute {
		t.Fatalf("busy: next round in %v", wait)
	}
	// Whatever happens, the next round is never due in the past.
	r = newRig(t)
	a = r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}, Stale: true}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	r.claim.during = func() { r.now = r.now.Add(3 * time.Hour) }
	if wait := r.d.Tick(context.Background()).Sub(r.now); wait < 2*time.Minute {
		t.Fatalf("next round in %v", wait)
	}
}

func TestAnAnswerAboutTheAccountAppliesToTheWholeBatch(t *testing.T) {
	// The session stops at the first offer when Epic's answer is about the
	// account. The offers behind it wait as long as that one, instead of
	// starting session after session ten minutes apart.
	for _, c := range []struct {
		outcome claim.Outcome
		wait    time.Duration
	}{
		{claim.NotSignedIn, 6 * time.Hour},
		{claim.Throttled, 25 * time.Hour},
		{claim.Captcha, 6 * time.Hour},
	} {
		r := newRig(t)
		a := r.offer("a", 5*24*time.Hour)
		b := r.offer("b", 6*24*time.Hour)
		r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a, b}}
		r.claim.results[a.Key()] = claim.Result{Outcome: c.outcome, Detail: "x"}
		r.tick()
		if e := r.entry("b"); e.NextAttempt.Sub(r.now) != c.wait || e.Attempts != 0 {
			t.Errorf("%s: the offer behind it waits %v (attempts %d), want %v", c.outcome, e.NextAttempt.Sub(r.now), e.Attempts, c.wait)
		}
		r.now = r.now.Add(time.Hour)
		r.tick()
		if len(r.claim.calls) != 1 {
			t.Errorf("%s: a second session ran an hour later", c.outcome)
		}
	}
}

func TestAStopHalfWayKeepsWhatWasFoundOut(t *testing.T) {
	r := newRig(t)
	a := r.offer("a", 5*24*time.Hour)
	b := r.offer("b", 6*24*time.Hour)
	c := r.offer("c", 6*24*time.Hour+time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a, b, c}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	r.claim.results[b.Key()] = claim.Result{Outcome: claim.Failed, Detail: "interrupted after the order button was pressed; the library will be checked next time"}
	ctx, cancel := context.WithCancel(context.Background())
	r.claim.during = cancel
	r.claim.err = context.Canceled
	r.d.Tick(ctx)
	if e := r.entry("a"); e.Status != state.Claimed {
		t.Errorf("the claim made before the stop was not recorded: %+v", e)
	}
	if e := r.entry("b"); e.Attempts != 1 || e.NextAttempt.Sub(r.now) != 20*time.Minute {
		t.Errorf("the interrupted order was not recorded: %+v", e)
	}
	if e := r.entry("c"); e.Attempts != 0 || !e.NextAttempt.IsZero() || !e.LastAttempt.IsZero() {
		t.Errorf("the offer that was never reached was touched: %+v", e)
	}
}

// ---- the daily timetable

func TestDailySlot(t *testing.T) {
	utc := DailySlot(16, 30, time.UTC)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for after, want := range map[string]string{
		"2026-11-12T10:00:00Z": "2026-11-12T16:30:00Z",
		"2026-11-12T16:29:59Z": "2026-11-12T16:30:00Z",
		"2026-11-12T16:30:00Z": "2026-11-13T16:30:00Z", // strictly after
		"2026-11-12T23:00:00Z": "2026-11-13T16:30:00Z",
		"2026-12-31T20:00:00Z": "2027-01-01T16:30:00Z",
	} {
		if got := utc(at(after)); !got.Equal(at(want)) {
			t.Errorf("after %s: %s, want %s", after, got.UTC().Format(time.RFC3339), want)
		}
	}
	// A local time of day keeps to the wall clock across a change to and
	// from daylight saving time.
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data")
	}
	local := DailySlot(18, 0, berlin)
	for after, want := range map[string]string{
		"2026-03-28T20:00:00+01:00": "2026-03-29T18:00:00+02:00", // clocks go forward that night
		"2026-10-24T20:00:00+02:00": "2026-10-25T18:00:00+01:00", // and back
		"2026-07-01T17:59:00+02:00": "2026-07-01T18:00:00+02:00",
	} {
		if got := local(at(after)); !got.Equal(at(want)) {
			t.Errorf("after %s: %s, want %s", after, got.In(berlin).Format(time.RFC3339), want)
		}
	}
	// A time of day that does not exist on the day the clocks go forward
	// still gives a moment later that day, not an endless loop.
	gap := DailySlot(2, 30, berlin)
	if got := gap(at("2026-03-28T20:00:00+01:00")); !got.After(at("2026-03-28T20:00:00+01:00")) || got.Sub(at("2026-03-28T20:00:00+01:00")) > 30*time.Hour {
		t.Errorf("slot in the missing hour: %s", got)
	}
}

func slotRig(t *testing.T) *rig {
	r := newRig(t)
	r.d.Slot = DailySlot(16, 30, time.UTC)
	return r
}

func TestChecksOnceADayAtTheSlot(t *testing.T) {
	r := slotRig(t) // the rig's clock stands at 10:00 UTC
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	if wait := r.tick(); wait != 6*time.Hour+30*time.Minute {
		t.Fatalf("next check in %v, want at 16:30", wait)
	}
	r.now = r.now.Add(6*time.Hour + 30*time.Minute)
	if wait := r.tick(); wait != 24*time.Hour {
		t.Fatalf("next check in %v, want a day later", wait)
	}
	// A giveaway that is due to start is looked at shortly after it starts,
	// not only at the next daily time.
	r.feed.snap.NextChange = r.now.Add(5 * time.Hour)
	if wait := r.tick(); wait != 5*time.Hour+2*time.Minute {
		t.Fatalf("next check in %v, want just after the giveaway starts", wait)
	}
}

func TestARestartDoesNotCauseACheck(t *testing.T) {
	r := slotRig(t)
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}

	// Never run before: look at once.
	if !r.d.Due().Equal(r.now) {
		t.Fatalf("first run due %v, want now", r.d.Due())
	}
	next := r.d.Tick(context.Background())
	if sc := r.d.State.Schedule(); !sc.NextDue.Equal(next) || !sc.LastCheck.Equal(r.now) {
		t.Fatalf("timetable not written down: %+v", sc)
	}

	// The program is started again (the PC was restarted, say), two hours
	// later: a new Daemon over the same state file.
	r.now = r.now.Add(2 * time.Hour)
	st, err := state.Open(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	again := &Daemon{Feed: r.feed, Claimer: r.claim, State: st, Slot: r.d.Slot, Now: func() time.Time { return r.now }, Jitter: r.d.Jitter, Logf: r.d.Logf}
	if !again.Due().Equal(next) {
		t.Fatalf("after a restart the next check is due %v, want still %v", again.Due(), next)
	}
	calls := r.feed.calls.Load()
	// Running, it waits for that time and has not looked at anything
	// merely because it was started.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { again.Run(ctx, RunOptions{}); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if r.feed.calls.Load() != calls {
		t.Fatal("the list was read merely because the program was started")
	}
}

func TestLooksEveryHourAndStraightAwayAfterANightOff(t *testing.T) {
	r := newRig(t) // every hour; the rig's clock stands at 10:00 UTC
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}

	// About an hour between looks: a little under to a little over, so
	// that not every PC asks Epic at the same minute.
	for _, j := range []time.Duration{0, 6 * time.Minute, 12*time.Minute - time.Second} {
		r.d.Jitter = func(max time.Duration) time.Duration {
			if j > max {
				return max
			}
			return j
		}
		if wait := r.tick(); wait < 54*time.Minute || wait > 66*time.Minute {
			t.Fatalf("next look in %v, want about an hour", wait)
		}
	}
	r.d.Jitter = func(time.Duration) time.Duration { return 0 }
	next := r.d.Tick(context.Background())

	// Started again twenty minutes later (the PC was restarted): it keeps
	// to the time it had.
	r.now = r.now.Add(20 * time.Minute)
	if !r.d.Due().Equal(next) {
		t.Fatalf("after a restart within the hour: due %v, want still %v", r.d.Due(), next)
	}
	// Started the next morning: that time is long past, so it looks now.
	r.now = r.now.Add(14 * time.Hour)
	if due := r.d.Due(); due.After(r.now) {
		t.Fatalf("after a night off: due in %v, want at once", due.Sub(r.now))
	}
	// A time further ahead than it ever plans (the clock was wrong when it
	// was written down) is not waited for.
	_ = r.d.State.SetSchedule(func(sc *state.Schedule) { sc.NextDue = r.now.Add(3 * time.Hour) })
	if due := r.d.Due(); !due.Equal(r.now) {
		t.Fatalf("a time three hours ahead was trusted: due in %v", due.Sub(r.now))
	}
	_ = r.d.State.SetSchedule(func(sc *state.Schedule) { sc.NextDue = r.now.Add(65 * time.Minute) })
	if due := r.d.Due(); !due.Equal(r.now.Add(65 * time.Minute)) {
		t.Fatalf("a time within the hour was not trusted: due in %v", due.Sub(r.now))
	}
	// A new giveaway is looked at minutes after it starts, not only at the
	// next hourly look; and never is the list asked for twice in a row.
	r.feed.snap.NextChange = r.now.Add(10 * time.Minute)
	if wait := r.tick(); wait != 12*time.Minute {
		t.Fatalf("next look in %v, want just after the giveaway starts", wait)
	}
	// With no interval given it is every hour, too.
	r.d.CheckEvery = 0
	r.feed.snap.NextChange = time.Time{}
	if wait := r.tick(); wait != 54*time.Minute {
		t.Fatalf("default interval: next look in %v", wait)
	}
}

func TestATimetableFromAWrongClockIsNotTrusted(t *testing.T) {
	r := slotRig(t)
	_ = r.d.State.SetSchedule(func(sc *state.Schedule) { sc.NextDue = r.now.Add(400 * 24 * time.Hour) })
	if due := r.d.Due(); !due.Equal(r.now) {
		t.Fatalf("due in %v, want now", due.Sub(r.now))
	}
	// Started by Windows exactly at the daily time (when "the next daily
	// time" is a whole day away), it must not leave without looking: the
	// round is what puts the timetable right.
	r.now = time.Date(2026, 11, 12, 16, 30, 0, 0, time.UTC)
	_ = r.d.State.SetSchedule(func(sc *state.Schedule) { sc.NextDue = r.now.Add(30 * 24 * time.Hour) })
	if !r.d.Due().Equal(r.now) {
		t.Fatalf("at the daily time: due %v", r.d.Due())
	}
	if wait := r.tick(); wait != 24*time.Hour {
		t.Fatalf("after the repairing round the next look is in %v", wait)
	}
	if sc := r.d.State.Schedule(); sc.NextDue.Sub(r.now) != 24*time.Hour {
		t.Fatalf("timetable not repaired: %v", sc.NextDue.Sub(r.now))
	}
}

func TestAnInterruptedRoundIsTakenUpAgain(t *testing.T) {
	r := slotRig(t)
	a := r.offer("a", 6*24*time.Hour)
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	_ = r.d.State.SetSchedule(func(sc *state.Schedule) { sc.NextDue = r.now.Add(-time.Minute) })
	ctx, cancel := context.WithCancel(context.Background())
	r.claim.during = cancel
	r.claim.err = context.Canceled
	r.d.Tick(ctx)
	if sc := r.d.State.Schedule(); !sc.NextDue.Equal(r.now.Add(-time.Minute)) {
		t.Fatalf("a round cut short moved the timetable to %v", sc.NextDue)
	}
}

func TestFailuresToReadTheListAreRememberedAcrossRestarts(t *testing.T) {
	r := slotRig(t)
	r.feed.err = errors.New("no network")
	if wait := r.tick(); wait != time.Minute {
		t.Fatalf("first failure: %v", wait)
	}
	st, err := state.Open(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	again := &Daemon{Feed: r.feed, Claimer: r.claim, State: st, Slot: r.d.Slot, Now: func() time.Time { return r.now }, Jitter: r.d.Jitter, Logf: r.d.Logf}
	if wait := again.Tick(context.Background()).Sub(r.now); wait != 2*time.Minute {
		t.Fatalf("second failure after a restart: %v", wait)
	}
	// After some hours of failures it is left until the next daily check.
	for i := 0; i < 20; i++ {
		again.Tick(context.Background())
	}
	if wait := again.Tick(context.Background()).Sub(r.now); wait != 6*time.Hour+30*time.Minute {
		t.Fatalf("after many failures: next try in %v, want at the daily time", wait)
	}
	// One success clears the count.
	r.feed.err = nil
	again.Tick(context.Background())
	if n := st.Schedule().FeedFailures; n != 0 {
		t.Fatalf("failures after a success = %d", n)
	}
}

func TestRunWaitsOutTheMinutesAfterSignIn(t *testing.T) {
	r := newRig(t)
	r.d.Now = time.Now
	holds := []time.Duration{150 * time.Millisecond, 0}
	var mu sync.Mutex
	settle := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		h := holds[0]
		if len(holds) > 1 {
			holds = holds[1:]
		}
		return h
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	go r.d.Run(ctx, RunOptions{Settle: settle})
	for r.feed.calls.Load() == 0 {
		if time.Since(started) > 5*time.Second {
			t.Fatal("the round never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if waited := time.Since(started); waited < 150*time.Millisecond {
		t.Fatalf("the round ran after %v, before the hold-off was over", waited)
	}
}

func TestAnAnswerAboutTheAccountCountsEvenWhenTheSessionWasCutShort(t *testing.T) {
	// The first offer's checkout says the account is throttled; then the
	// user opens the launcher and the session is handed over (or the
	// program is told to stop). The offer behind it must not be tried
	// again half an hour later.
	for name, sessionErr := range map[string]error{"handed over": ErrYielded, "stopped": context.Canceled} {
		r := newRig(t)
		a := r.offer("a", 5*24*time.Hour)
		b := r.offer("b", 6*24*time.Hour)
		r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a, b}}
		r.claim.results[a.Key()] = claim.Result{Outcome: claim.Throttled, Detail: "x"}
		r.claim.err = sessionErr
		ctx, cancel := context.WithCancel(context.Background())
		if sessionErr == context.Canceled {
			r.claim.during = cancel
		}
		r.d.Tick(ctx)
		cancel()
		if e := r.entry("b"); e.NextAttempt.Sub(r.now) != 25*time.Hour || e.Attempts != 0 {
			t.Errorf("%s: the offer behind it waits %v (attempts %d), want 25h", name, e.NextAttempt.Sub(r.now), e.Attempts)
		}
	}
}

// ---- when the list cannot be read, and when the PC's clock is wrong

func TestGoesByTheLastListWhenTheListCannotBeRead(t *testing.T) {
	r := slotRig(t)
	a := r.offer("a", 6*24*time.Hour) // will be retried from memory
	b := r.offer("b", 30*time.Minute) // will have ended by then
	c := r.offer("c", 6*24*time.Hour) // already claimed
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a, b, c}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Failed, Detail: "the checkout did not become ready"}
	r.claim.results[b.Key()] = claim.Result{Outcome: claim.Failed, Detail: "the checkout did not become ready"}
	r.claim.results[c.Key()] = claim.Result{Outcome: claim.Claimed}
	r.tick()
	if len(r.claim.calls) != 1 {
		t.Fatalf("sessions = %d", len(r.claim.calls))
	}

	// An hour later Epic's list cannot be read. The giveaway that failed
	// earlier is due again and is still running: it is tried all the same.
	r.now = r.now.Add(time.Hour)
	readAt := r.now.Add(-time.Hour)
	r.feed.err = errors.New("no such host")
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	// Not at the first failed read, though: a PC that has just woken up is
	// often simply not online yet, and the list is asked for again in a
	// minute or two.
	for i := 0; i < 2; i++ {
		wait := r.tick()
		if len(r.claim.calls) != 1 {
			t.Fatalf("a session was started from memory after only %d failed read(s)", i+1)
		}
		r.now = r.now.Add(wait)
	}
	wait := r.tick()
	if len(r.claim.calls) != 2 || len(r.claim.calls[1]) != 1 || r.claim.calls[1][0] != "Game a" {
		t.Fatalf("sessions = %v", r.claim.calls)
	}
	if e := r.entry("a"); e.Status != state.Claimed {
		t.Fatalf("status of a = %s", e.Status)
	}
	// The list itself is asked for again soon, not at the daily time.
	if wait > 5*time.Minute {
		t.Fatalf("list read again in %v", wait)
	}
	if !r.logged("going by the list as read on") {
		t.Errorf("logs = %q", r.logs)
	}
	// The timetable does not count this as a successful look.
	if sc := r.d.State.Schedule(); !sc.LastCheck.Equal(readAt) {
		t.Errorf("last successful read = %v", sc.LastCheck)
	}
	// With nothing left to go by, nothing is started.
	r.now = r.now.Add(time.Hour)
	r.tick()
	if len(r.claim.calls) != 2 {
		t.Fatalf("a session was started with nothing open: %v", r.claim.calls)
	}
}

func TestGoesByEpicsClockWhenThePCsIsFarOff(t *testing.T) {
	// The PC's clock is eight days ahead. By it, the giveaway below ended
	// days ago; by Epic's clock (which the list reader reports the
	// difference to) it runs for another two days.
	r := slotRig(t)
	skew := -8 * 24 * time.Hour
	epicNow := r.now.Add(skew)
	a := feed.Offer{Title: "Game a", Namespace: "ns", OfferID: "a", Start: epicNow.Add(-time.Hour), End: epicNow.Add(48 * time.Hour)}
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}, ClockSkew: skew, NextChange: epicNow.Add(48 * time.Hour)}
	var passed []feed.Offer
	r.claim.inspect = func(offers []feed.Offer) { passed = append(passed, offers...) }
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	wait := r.tick()
	if len(passed) != 1 {
		t.Fatalf("the giveaway was not attempted: %v", r.claim.calls)
	}
	// What is passed on carries its end in the PC's own time, so that
	// "how long is left" comes out right downstream.
	if left := passed[0].End.Sub(r.now); left != 48*time.Hour {
		t.Errorf("time left as seen downstream = %v", left)
	}
	if passed[0].Key() != a.Key() {
		t.Errorf("the giveaway's identity changed: %s", passed[0].Key())
	}
	if e := r.entry("a"); e.Status != state.Claimed {
		t.Errorf("status = %s", e.Status)
	}
	// The next look is at the daily time, by the PC's clock, not a week off.
	if wait != 6*time.Hour+30*time.Minute {
		t.Errorf("next look in %v", wait)
	}
	if !r.logged("clock differs from Epic's") {
		t.Errorf("logs = %q", r.logs)
	}
}

func TestSaysWhenAnotherCountrysListWasUsed(t *testing.T) {
	r := slotRig(t)
	r.d.Country = "XK"
	r.feed.snap = feed.Snapshot{Country: "US"}
	r.tick()
	if !r.logged("no free-games list for country") {
		t.Errorf("logs = %q", r.logs)
	}
}

func TestListRetriesStartAfreshAtTheCheckTheyWereLeftTo(t *testing.T) {
	// A long outage uses the quick retries up; the rest is left to the
	// next daily check. If the read fails then as well (the PC has only
	// just woken up, say), it is the first failure of a new series and is
	// retried after a minute, not a day later.
	r := slotRig(t) // 10:00 UTC, daily slot 16:30 UTC
	r.feed.err = errors.New("no network")
	slot := r.now.Add(6*time.Hour + 30*time.Minute)
	for i := 0; i < 30; i++ {
		next := r.d.Tick(context.Background())
		if !next.After(r.now) {
			t.Fatalf("next round not in the future: %v", next.Sub(r.now))
		}
		if next.After(slot) {
			t.Fatalf("round %d planned beyond the daily check: %v", i, next.Sub(r.now))
		}
		r.now = next
		if r.now.Equal(slot) {
			break
		}
	}
	if !r.now.Equal(slot) {
		t.Fatalf("never arrived at the daily check; now %v", r.now)
	}
	if wait := r.tick(); wait != time.Minute {
		t.Fatalf("a failure at the daily check is retried in %v, want 1m", wait)
	}
	// And one success clears everything.
	r.feed.err = nil
	r.tick()
	if sc := r.d.State.Schedule(); sc.FeedFailures != 0 || !sc.FeedGaveUpUntil.IsZero() {
		t.Fatalf("after a success: %+v", sc)
	}
}

func TestStoredTimesAreInThePCsClock(t *testing.T) {
	// PC three days behind Epic. The giveaway runs for another twelve
	// hours. What is written down (the record's end, the remembered list)
	// must be in the PC's time, because that is what it is compared with
	// later, also when the list cannot be read and no difference is known.
	r := slotRig(t)
	skew := 3 * 24 * time.Hour
	epicNow := r.now.Add(skew)
	a := feed.Offer{Title: "Game a", Namespace: "ns", OfferID: "a", Start: epicNow.Add(-time.Hour), End: epicNow.Add(12 * time.Hour)}
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}, ClockSkew: skew}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Failed, Detail: "x"}
	r.tick()
	if e := r.entry("a"); e.Ends.Sub(r.now) != 12*time.Hour {
		t.Fatalf("recorded end is %v from now, want 12h", e.Ends.Sub(r.now))
	}
	// Thirteen hours later the giveaway is over. The list cannot be read:
	// nothing is attempted from memory.
	r.now = r.now.Add(13 * time.Hour)
	r.feed.err = errors.New("no network")
	for i := 0; i < 5; i++ {
		r.now = r.now.Add(r.tick())
	}
	if len(r.claim.calls) != 1 {
		t.Fatalf("an ended giveaway was attempted from memory: %v", r.claim.calls)
	}
}

func TestRunSaysWhenItIsWaiting(t *testing.T) {
	// The program makes itself small during waits: Run announces each wait
	// and its end, and never starts a round while "waiting".
	r := newRig(t)
	r.d.Now = time.Now
	var mu sync.Mutex
	var events []string
	waiting := false
	ran := make(chan struct{})
	var ranOnce sync.Once
	r.claim.during = func() {
		mu.Lock()
		if waiting {
			events = append(events, "ROUND WHILE WAITING")
		}
		mu.Unlock()
		ranOnce.Do(func() { close(ran) })
	}
	a := feed.Offer{Title: "Game a", Namespace: "ns", OfferID: "a", Start: time.Now().Add(-time.Hour), End: time.Now().Add(48 * time.Hour)}
	r.feed.snap = feed.Snapshot{FreeNow: []feed.Offer{a}}
	r.claim.results[a.Key()] = claim.Result{Outcome: claim.Claimed}
	_ = r.d.State.SetSchedule(func(sc *state.Schedule) { sc.NextDue = time.Now().Add(6 * time.Second) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.d.Run(ctx, RunOptions{Waiting: func(w bool) {
			mu.Lock()
			waiting = w
			if w {
				events = append(events, "wait")
			} else {
				events = append(events, "awake")
			}
			mu.Unlock()
		}})
		close(done)
	}()
	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("the round never ran")
	}
	// Give it time to get into the wait for the next day (however slow the
	// machine): look until that wait has been announced.
	for i := 0; i < 500; i++ {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(events, ",")
	if got != "wait,awake,wait,awake" {
		t.Fatalf("events = %s", got)
	}
}
