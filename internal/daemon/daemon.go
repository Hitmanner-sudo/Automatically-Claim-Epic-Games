// Package daemon is the scheduling part: it reads Epic's free-games list
// every hour (and right after a new giveaway is due to start), decides when
// to try claiming what, and remembers its timetable so that being started
// again changes nothing about when it next looks.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrUnsupported means a launcher was found that this program cannot drive.
var ErrUnsupported = errors.New("this Epic Games Launcher cannot be driven")

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

	Country string
	// CheckEvery is how often the list is read: every hour unless set.
	CheckEvery time.Duration
	// Slot, when set, replaces the interval by a timetable: it gives the
	// next regular check time after a moment (see DailySlot, for once a
	// day at a set time).
	Slot func(after time.Time) time.Time

	// Injectable for tests.
	Now    func() time.Time
	Jitter func(max time.Duration) time.Duration

	lastBusy   string
	lastNotice string
}

// DailySlot returns a function that gives the next daily check time after a
// moment: at hour:minute in loc, every day.
func DailySlot(hour, minute int, loc *time.Location) func(after time.Time) time.Time {
	return func(after time.Time) time.Time {
		a := after.In(loc)
		for day := 0; ; day++ {
			// time.Date normalises the day overflow and applies the zone's
			// rules for that date (daylight saving included).
			t := time.Date(a.Year(), a.Month(), a.Day()+day, hour, minute, 0, 0, loc)
			if t.After(after) {
				return t
			}
		}
	}
}

// regular is the next regular check after now.
func (d *Daemon) regular(now time.Time) time.Time {
	if d.Slot != nil {
		return d.Slot(now).Add(d.jitter(10 * time.Minute))
	}
	every := d.every()
	return now.Add(every - every/10 + d.jitter(every/5))
}

func (d *Daemon) every() time.Duration {
	if d.CheckEvery > 0 {
		return d.CheckEvery
	}
	return time.Hour
}

// horizon is further ahead than any round is ever planned.
func (d *Daemon) horizon() time.Duration {
	if d.Slot != nil {
		return 26 * time.Hour
	}
	return 2*d.every() + 10*time.Minute
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
	laterRetry   = 24 * time.Hour
	// Epic's own notice asks to wait 24 hours.
	throttleRetry = 25 * time.Hour
	staleRetry    = 5 * time.Minute
	// The list is cached for about two minutes at Epic's edge; asking more
	// often than that is pointless.
	minSleep = 2 * time.Minute
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
// run next. That time is also written down, so that a later start of the
// program carries on from it.
func (d *Daemon) Tick(ctx context.Context) time.Time {
	next, read := d.round(ctx)
	if ctx.Err() != nil {
		// Cut short by a stop request: the timetable stays as it was, so
		// that the round is taken up again at the next start.
		return next
	}
	_ = d.State.SetSchedule(func(sc *state.Schedule) {
		sc.NextDue = next
		if read {
			sc.LastCheck = d.now()
		}
	})
	return next
}

// Due is when the next round is due according to what was written down the
// last time. A restart of the program, or of the PC, therefore does not by
// itself cause a round: a program that has never run looks at once, and so
// does one that finds its time has passed (the PC was off for the night);
// one that was started again within the hour waits for its time.
func (d *Daemon) Due() time.Time {
	now := d.now()
	sc := d.State.Schedule()
	switch {
	case sc.NextDue.IsZero():
		return now
	case sc.NextDue.Sub(now) > d.horizon():
		// Further away than any round is ever planned: the clock must have
		// been wrong when it was written down. Look now; that round writes
		// a sound timetable again.
		return now
	}
	return sc.NextDue
}

// feedRetry is how long to wait after the n-th consecutive failure to read
// the list: soon at first (a PC that has just woken up may not be online
// yet), then more slowly, and after a few hours not before the next regular
// check.
// rememberAfter is how many reads in a row must have failed before the
// list as last read is gone by.
const rememberAfter = 3

var feedRetry = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, time.Hour, time.Hour, time.Hour}

// lastList is the free-games list as last read, kept in the state file.
type lastList struct {
	ReadAt  time.Time    `json:"readAt"`
	FreeNow []feed.Offer `json:"freeNow"`
}

func (d *Daemon) remember(snap feed.Snapshot, now time.Time) {
	if raw, err := json.Marshal(lastList{ReadAt: now, FreeNow: snap.FreeNow}); err == nil {
		_ = d.State.SetLastList(raw)
	}
}

// remembered returns the giveaways of the list as last read that are still
// running and not settled: what there is to go by while the list cannot be
// read. (They were free when it was read, a giveaway runs until its stated
// end, and the checkout has the last word on the price in any case.)
func (d *Daemon) remembered(now time.Time) (offers []feed.Offer, readAt time.Time) {
	var l lastList
	if raw := d.State.LastList(); len(raw) == 0 || json.Unmarshal(raw, &l) != nil {
		return nil, time.Time{}
	}
	for _, o := range l.FreeNow {
		if o.Namespace == "" || o.OfferID == "" || o.DiscountPrice != 0 || !now.Before(o.End) {
			continue
		}
		if e, ok := d.State.Get(o.Key()); ok && !e.Status.Done() {
			offers = append(offers, o)
		}
	}
	return offers, l.ReadAt
}

func (d *Daemon) round(ctx context.Context) (next time.Time, read bool) {
	now := d.now()

	snap, err := d.Feed.Fetch(ctx, d.Country, "en-US", now)
	read = err == nil
	var listRetry time.Time
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}, false
		}
		failures := 0
		var gaveUpUntil time.Time
		_ = d.State.SetSchedule(func(sc *state.Schedule) {
			if !sc.FeedGaveUpUntil.IsZero() && !now.Before(sc.FeedGaveUpUntil) {
				// This is the regular check the retries were left to: a
				// failure now is the first of a new series.
				sc.FeedFailures, sc.FeedGaveUpUntil = 0, time.Time{}
			}
			sc.FeedFailures++
			failures, gaveUpUntil = sc.FeedFailures, sc.FeedGaveUpUntil
		})
		// Meanwhile, giveaways known from the last time the list was read
		// can still be claimed.
		known, readAt := d.remembered(now)
		if failures < rememberAfter {
			// A PC that has just woken up is often simply not online yet;
			// the list is asked for again in a minute or two anyway.
			known = nil
		}
		meanwhile := ""
		if len(known) > 0 {
			meanwhile = "; meanwhile going by the list as read on " + readAt.Local().Format("Mon 2 Jan 15:04") + " for what is still open"
		}
		listRetry = d.regular(now)
		if failures > len(feedRetry) {
			if gaveUpUntil.IsZero() {
				gaveUpUntil = listRetry
				_ = d.State.SetSchedule(func(sc *state.Schedule) { sc.FeedGaveUpUntil = gaveUpUntil })
			}
			if gaveUpUntil.After(now) {
				listRetry = gaveUpUntil
			}
			d.notice("feed-giving-up", "the free-games list could still not be read after %d tries; leaving it until the next regular check%s", failures, meanwhile)
		} else {
			if at := now.Add(feedRetry[failures-1]); at.Before(listRetry) {
				listRetry = at
			}
			key := "feed:" + err.Error()
			if meanwhile != "" {
				key += "+memory" // said once more when it starts going by memory
			}
			d.notice(key, "could not read the free-games list (%v); trying again in %v%s", err, listRetry.Sub(now).Round(time.Minute), meanwhile)
		}
		if len(known) == 0 {
			return listRetry, false
		}
		snap = feed.Snapshot{FreeNow: known}
	} else {
		if sc := d.State.Schedule(); sc.FeedFailures != 0 || !sc.FeedGaveUpUntil.IsZero() {
			_ = d.State.SetSchedule(func(sc *state.Schedule) { sc.FeedFailures, sc.FeedGaveUpUntil = 0, time.Time{} })
		}
		_ = d.State.Prune(now, 60*24*time.Hour)
		// With a PC clock that is far off, Epic's time has decided what is
		// running. From here on every time is in the PC's own clock, which
		// is what everything else (and everything stored) is compared with.
		// A giveaway's start stays as it is: it is part of its identity.
		if skew := snap.ClockSkew; skew != 0 {
			free := make([]feed.Offer, len(snap.FreeNow))
			for i, o := range snap.FreeNow {
				o.End = o.End.Add(-skew)
				free[i] = o
			}
			snap.FreeNow = free
			if !snap.NextChange.IsZero() {
				snap.NextChange = snap.NextChange.Add(-skew)
			}
			d.notice("clock", "this PC's clock differs from Epic's by %v; going by Epic's time to tell what is free", (-skew).Round(time.Minute))
		}
		d.remember(snap, now)
		if snap.Country != "" && d.Country != "" && !strings.EqualFold(snap.Country, d.Country) && !(strings.EqualFold(d.Country, "UK") && snap.Country == "GB") {
			d.notice("feed-country", "Epic has no free-games list for country %q; going by the one for %s (set \"country\" in config.json to your Epic account's country)", d.Country, snap.Country)
		}
	}
	var due []feed.Offer
	next = d.regular(now)
	if !read {
		next = listRetry
	}
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
		// A session can take many minutes; what follows counts from its end.
		now = d.now()
	}

	// Epic's servers lag a few minutes behind a changeover; look again soon
	// while the list is visibly behind.
	if snap.Stale {
		sooner(now.Add(staleRetry))
	}
	// Wake up shortly after the list is due to change (a new giveaway
	// starting), with a few minutes' grace for Epic's servers to catch up.
	if !snap.NextChange.IsZero() && snap.NextChange.After(now) {
		sooner(snap.NextChange.Add(2*time.Minute + d.jitter(5*time.Minute)))
	}
	if earliest := now.Add(minSleep); next.Before(earliest) {
		next = earliest
	}
	return next, read
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
	// Waiting periods count from the end of the session, which may have
	// taken a good while.
	now = d.now()

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
	sessionWait, sessionNote, countAttempt, cancelled := time.Duration(0), "", false, false
	// An answer that concerns the account rather than one offer ends the
	// session early; the offers behind it wait just as long, however the
	// session ended otherwise.
	acctWait, acctNote := time.Duration(0), ""
	for _, res := range results {
		w := time.Duration(0)
		switch res.Outcome {
		case claim.NotSignedIn:
			w = signInRetry
		case claim.Throttled:
			w = throttleRetry
		case claim.Captcha:
			w = captchaBackoff[0]
		}
		if w > acctWait {
			acctWait, acctNote = w, res.String()
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrNotInstalled), errors.Is(err, ErrUnsupported):
		sessionWait, sessionNote = missingRetry, err.Error()
		d.notice("missing", "%v; will look again in %v", err, missingRetry)
	case errors.Is(err, claim.ErrNotSignedIn):
		sessionWait, sessionNote = signInRetry, err.Error()
		d.notice("signin", "%v; will try again in %v", err, signInRetry)
	case errors.Is(err, ErrYielded):
		sessionWait, sessionNote = yieldRetry, err.Error()
		d.logf("%v; will try again in %v", err, yieldRetry)
	case ctx.Err() != nil:
		// Asked to stop half-way: what the session did find out is still
		// recorded; the offers it never reached are left as they were.
		cancelled = true
	default:
		sessionNote, countAttempt = err.Error(), true
		d.logf("claim session failed: %v", err)
	}
	if err == nil || countAttempt {
		d.lastNotice = ""
	}
	if acctWait > sessionWait {
		sessionWait, sessionNote, countAttempt = acctWait, acctNote, false
	}

	for _, o := range due {
		key := o.Key()
		res, reached := results[key]
		if !reached {
			if cancelled && acctWait == 0 {
				continue
			}
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
				// The checkout disagreed with the free-games list about
				// the price. Nothing was ordered, and looking again costs
				// nothing, so it is retried, but only rarely.
				e.Attempts++
				e.NextAttempt = now.Add(pick(captchaBackoff, e.Attempts))
				d.logf("%s was NOT ordered: %s; will look again %s", o.Title, res.Detail, e.NextAttempt.Local().Format("Mon 15:04"))
			case claim.Later:
				e.NextAttempt = now.Add(laterRetry)
				d.logf("%s: %s; will look again in %v", o.Title, res.Detail, laterRetry)
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

// RunOptions say how Run behaves around its rounds.
type RunOptions struct {
	// Settle returns how much longer to hold off because the user has only
	// just signed in to Windows: the PC is busy then, and nothing should
	// happen merely because it was turned on. Nil means never hold off.
	Settle func() time.Duration
	// Waiting is called with true before a wait between rounds and with
	// false when it is over, so that the program can make itself small
	// meanwhile. May be nil.
	Waiting func(waiting bool)
}

// Run does the rounds that are due, one after the other, until the context
// ends.
func (d *Daemon) Run(ctx context.Context, opt RunOptions) {
	next := d.Due()
	for {
		if wait := next.Sub(d.now()); wait > 5*time.Second && opt.Waiting != nil {
			opt.Waiting(true)
			ok := sleepUntil(ctx, next, d.now)
			opt.Waiting(false)
			if !ok {
				return
			}
		} else if !sleepUntil(ctx, next, d.now) {
			return
		}
		if opt.Settle != nil {
			if hold := opt.Settle(); hold > 0 {
				next = d.now().Add(hold)
				continue
			}
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
		d.logf("next check %s", next.Local().Format("Mon 2 Jan 15:04"))
	}
}

// sleepUntil waits for a wall-clock time. It checks the clock in short
// steps instead of sleeping in one go, so that a PC waking from standby
// (during which timers do not advance) notices at once that the time has
// come. It returns false when the context ends first.
func sleepUntil(ctx context.Context, when time.Time, now func() time.Time) bool {
	// One look at the clock a minute is all a wait costs.
	const step = time.Minute
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
