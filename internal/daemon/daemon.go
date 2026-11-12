// Package daemon is the always-running part: it watches Epic's free-games
// list and decides when to try claiming what.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/state"
)

// BusyError means a claim session could not start right now because the
// launcher (or the PC) is in use. It is a postponement, not a failure.
type BusyError struct{ Reason string }

func (e *BusyError) Error() string { return "postponed: " + e.Reason }

// ErrYielded means a session was abandoned because the user asked for the
// launcher while it was running hidden.
var ErrYielded = errors.New("the launcher was opened by the user; stepping aside")

// ErrNotInstalled means the launcher could not be found.
var ErrNotInstalled = errors.New("the Epic Games Launcher is not installed")

// Feed fetches the free-games list.
type Feed interface {
	Fetch(ctx context.Context, country, locale string, now time.Time) (feed.Snapshot, error)
}

// Claimer claims a batch of offers in one hidden launcher session. It
// returns one result per offer key for the offers it got to. A non-nil error
// describes why the session as a whole did not happen or was cut short.
type Claimer interface {
	ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error)
}

// Daemon ties the pieces together.
type Daemon struct {
	Feed    Feed
	Claimer Claimer
	State   *state.Store
	Logf    func(format string, args ...any)

	Country    string
	CheckEvery time.Duration

	// Injectable for tests.
	Now    func() time.Time
	Jitter func(max time.Duration) time.Duration

	feedFailures int
	lastBusy     string
	lastNotice   string
}

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Daemon) jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	if d.Jitter != nil {
		return d.Jitter(max)
	}
	return time.Duration(rand.Int64N(int64(max)))
}

func (d *Daemon) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// notice logs a line unless it repeats the previous notice, so that a
// condition that lasts for hours is reported once.
func (d *Daemon) notice(key, format string, args ...any) {
	if d.lastNotice == key {
		return
	}
	d.lastNotice = key
	d.logf(format, args...)
}

// Back-off schedules. Promotions last a week, so patience costs nothing.
var (
	failBackoff    = []time.Duration{20 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour}
	captchaBackoff = []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour}
)

const (
	signInRetry  = 6 * time.Hour
	missingRetry = 12 * time.Hour
	busyRetry    = 10 * time.Minute
	yieldRetry   = 30 * time.Minute
	dryRunRetry  = 24 * time.Hour
	// Epic's own notice asks to wait 24 hours.
	throttleRetry = 25 * time.Hour
	minSleep      = time.Minute
	// endMargin: no order is started this close to a promotion's end.
	endMargin = 3 * time.Minute
)

func pick(schedule []time.Duration, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(schedule) {
		attempts = len(schedule)
	}
	return schedule[attempts-1]
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Tick does one round: read the list, claim what is due, and return when to
// run next.
func (d *Daemon) Tick(ctx context.Context) time.Time {
	now := d.now()
	every := d.CheckEvery
	if every <= 0 {
		every = time.Hour
	}

	snap, err := d.Feed.Fetch(ctx, d.Country, "en-US", now)
	if err != nil {
		d.feedFailures++
		wait := time.Duration(d.feedFailures) * 2 * time.Minute
		if wait > every {
			wait = every
		}
		d.notice("feed:"+err.Error(), "could not read the free-games list (%v); trying again in %v", err, wait.Round(time.Minute))
		return now.Add(wait)
	}
	d.feedFailures = 0
	_ = d.State.Prune(now, 60*24*time.Hour)

	var due []feed.Offer
	next := now.Add(every - every/10 + d.jitter(every/5))
	sooner := func(t time.Time) {
		if t.Before(next) {
			next = t
		}
	}

	for _, o := range snap.FreeNow {
		key := o.Key()
		entry, known := d.State.Get(key)
		if !known {
			d.logf("free now: %s (until %s)", o.Title, o.End.Local().Format("Mon 2 Jan 15:04"))
			_ = d.State.Update(key, func(e *state.Entry) {
				e.Title, e.FirstSeen, e.Ends = o.Title, now, o.End
			})
			entry, _ = d.State.Get(key)
		}
		if entry.Status.Done() {
			continue
		}
		if o.Mobile {
			d.logf("%s is a mobile-only giveaway and cannot be claimed from a PC; skipping", o.Title)
			_ = d.State.Update(key, func(e *state.Entry) {
				e.Status, e.DoneAt, e.LastResult = state.Skipped, now, "mobile-only offer"
			})
			continue
		}
		if o.End.Sub(now) < endMargin {
			continue
		}
		if now.Before(entry.NextAttempt) {
			sooner(entry.NextAttempt)
			continue
		}
		due = append(due, o)
	}

	if len(due) > 0 {
		sort.SliceStable(due, func(i, j int) bool { return due[i].End.Before(due[j].End) })
		if t := d.claim(ctx, due); !t.IsZero() {
			sooner(t)
		}
	}

	// Wake up shortly after the list is due to change (a new giveaway
	// starting), with a few minutes' grace for Epic's servers to catch up.
	if !snap.NextChange.IsZero() && snap.NextChange.After(now) {
		sooner(snap.NextChange.Add(2*time.Minute + d.jitter(5*time.Minute)))
	}
	if earliest := now.Add(minSleep); next.Before(earliest) {
		next = earliest
	}
	return next
}

// claim runs one session for the due offers, records the outcomes and
// returns the earliest time any of them should be looked at again.
func (d *Daemon) claim(ctx context.Context, due []feed.Offer) time.Time {
	now := d.now()
	var earliest time.Time
	retryAt := func(t time.Time) {
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	names := make([]string, len(due))
	for i, o := range due {
		names[i] = o.Title
	}

	results, err := d.Claimer.ClaimAll(ctx, due)

	var busy *BusyError
	if errors.As(err, &busy) {
		// Nothing was attempted; do not count it against the offers.
		if d.lastBusy != busy.Reason {
			d.lastBusy = busy.Reason
			d.logf("waiting to claim %s: %s", strings.Join(names, ", "), busy.Reason)
		}
		return now.Add(busyRetry)
	}
	d.lastBusy = ""

	// A session-level problem applies to every offer the session did not
	// reach.
	sessionWait, sessionNote, countAttempt := time.Duration(0), "", false
	switch {
	case err == nil:
	case errors.Is(err, ErrNotInstalled):
		sessionWait, sessionNote = missingRetry, err.Error()
		d.notice("missing", "%v; will look again in %v", err, missingRetry)
	case errors.Is(err, claim.ErrNotSignedIn):
		sessionWait, sessionNote = signInRetry, err.Error()
		d.notice("signin", "%v; will try again in %v", err, signInRetry)
	case errors.Is(err, ErrYielded):
		sessionWait, sessionNote = yieldRetry, err.Error()
		d.logf("%v; will try again in %v", err, yieldRetry)
	case ctx.Err() != nil:
		return time.Time{}
	default:
		sessionNote, countAttempt = err.Error(), true
		d.logf("claim session failed: %v", err)
	}
	if err == nil || countAttempt {
		d.lastNotice = ""
	}

	for _, o := range due {
		key := o.Key()
		res, reached := results[key]
		if !reached {
			_ = d.State.Update(key, func(e *state.Entry) {
				wait := sessionWait
				if countAttempt {
					e.Attempts++
					wait = pick(failBackoff, e.Attempts)
				}
				if wait == 0 {
					wait = busyRetry
				}
				e.LastAttempt, e.NextAttempt, e.LastResult = now, now.Add(wait), short(sessionNote)
				retryAt(e.NextAttempt)
			})
			continue
		}
		_ = d.State.Update(key, func(e *state.Entry) {
			e.LastAttempt, e.LastResult = now, short(res.String())
			switch res.Outcome {
			case claim.Claimed:
				e.Status, e.DoneAt = state.Claimed, now
				d.logf("CLAIMED: %s (%s)", o.Title, res.Detail)
			case claim.Owned:
				e.Status, e.DoneAt = state.Owned, now
				d.logf("%s is already in the library", o.Title)
			case claim.Unavailable:
				e.Status, e.DoneAt = state.Skipped, now
				d.logf("%s cannot be claimed on this account: %s", o.Title, res.Detail)
			case claim.NotFree:
				// Never retried within this promotion: the checkout
				// disagreed with the free-games list about the price.
				e.Status, e.DoneAt = state.Skipped, now
				d.logf("%s was NOT ordered: %s", o.Title, res.Detail)
			case claim.Captcha:
				e.Attempts++
				e.NextAttempt = now.Add(pick(captchaBackoff, e.Attempts))
				d.logf("%s: %s; leaving it alone until %s", o.Title, res.Detail, e.NextAttempt.Local().Format("Mon 15:04"))
			case claim.Throttled:
				e.NextAttempt = now.Add(throttleRetry)
				d.logf("%s: %s; trying again in %v", o.Title, res.Detail, throttleRetry)
			case claim.NotSignedIn:
				e.NextAttempt = now.Add(signInRetry)
				d.notice("signin", "%s: %s; will try again in %v", o.Title, res.Detail, signInRetry)
			case claim.DryRun:
				e.NextAttempt = now.Add(dryRunRetry)
				d.logf("%s: %s", o.Title, res.Detail)
			default:
				e.Attempts++
				e.NextAttempt = now.Add(pick(failBackoff, e.Attempts))
				d.logf("%s: attempt %d failed: %s; next try %s", o.Title, e.Attempts, res.Detail, e.NextAttempt.Local().Format("Mon 15:04"))
			}
			if !e.Status.Done() {
				retryAt(e.NextAttempt)
			}
		})
	}
	return earliest
}

// Run loops until the context ends. startDelay postpones the first round
// (after a login the PC is busy enough already).
func (d *Daemon) Run(ctx context.Context, startDelay time.Duration) {
	next := d.now().Add(startDelay)
	for {
		if !sleepUntil(ctx, next, d.now) {
			return
		}
		func() {
			defer func() {
				// One bad round must not end the program.
				if r := recover(); r != nil {
					d.logf("internal error in this round: %v", r)
					next = d.now().Add(15 * time.Minute)
				}
			}()
			next = d.Tick(ctx)
		}()
		if ctx.Err() != nil {
			return
		}
		d.logf("next check %s", fmt.Sprint(next.Local().Format("Mon 2 Jan 15:04")))
	}
}

// sleepUntil waits for a wall-clock time. It checks the clock in short
// steps instead of sleeping in one go, so that a PC waking from standby
// (during which timers do not advance) notices at once that the time has
// come. It returns false when the context ends first.
func sleepUntil(ctx context.Context, when time.Time, now func() time.Time) bool {
	const step = 20 * time.Second
	for {
		remaining := when.Sub(now())
		if remaining <= 0 {
			return ctx.Err() == nil
		}
		if remaining > step {
			remaining = step
		}
		t := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}
