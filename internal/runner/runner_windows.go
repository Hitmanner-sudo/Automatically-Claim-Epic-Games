//go:build windows

// Package runner carries out one claim session on Windows: it makes room
// for a hidden launcher, starts it on a private desktop, drives the checkout
// through its DevTools endpoint, and puts everything back as it was.
package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/daemon"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/launcher"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

// unsupportedNote explains why Epic's new-generation launcher cannot be
// used. Tested against its preview 0.115 on a real Windows machine: it
// ignores the browser-debugging switch and exits when started on a desktop
// that is not displayed.
const unsupportedNote = "the new-generation Epic Games Launcher (preview) is installed, which does not offer what this program needs; it works with the classic launcher from Epic's regular installer"

// Runner implements daemon.Claimer.
type Runner struct {
	LauncherPath        string
	RestartIdleLauncher bool
	DryRun              bool
	// Again: see claim.Options.Again.
	Again   bool
	DiagDir string
	Logf    func(format string, args ...any)

	mu sync.Mutex // one session at a time
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// sessionLock keeps two copies of this program (the background process and
// a command run by hand) from running hidden launchers at the same time and
// taking each other's for the user's.
const sessionLock = `Local\EpicAutoClaim-session`

// errUserAsked means the user asked for the launcher; it has been started
// for them and the session is over.
var errUserAsked = errors.New("the user asked for the launcher")

// hidden is a running hidden launcher together with the watch that notices
// the user asking for the launcher and keeps stray windows off their screen.
type hidden struct {
	in   *launcher.Instance
	base string

	ctx    context.Context // ends when the user asks for the launcher
	cancel context.CancelFunc

	watch   sync.WaitGroup
	userCmd atomic.Pointer[string]

	seen *seen
}

// seen collects, over a whole session (all its start attempts and update
// rounds), what should never happen: launcher windows that reached the
// visible desktop. Each was hidden again at once; they are kept so that a
// trial can report them.
type seen struct {
	mu sync.Mutex
	// leaked are windows of a hidden launcher, or of one left over from an
	// update round, that turned up on the visible desktop.
	leaked []win.Window
	// flickers counts windows the launcher tried to show while it was
	// installing an update of its own in the tray.
	flickers int
}

func (s *seen) add(ws ...win.Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range ws {
		known := false
		for _, k := range s.leaked {
			known = known || k.HWND == w.HWND
		}
		if !known {
			s.leaked = append(s.leaked, w)
		}
	}
}

func (s *seen) report() (leaked []win.Window, flickers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]win.Window(nil), s.leaked...), s.flickers
}

func (r *Runner) startWatch(h *hidden) {
	h.watch.Add(1)
	go func() {
		defer h.watch.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		warned := false
		for {
			select {
			case <-h.ctx.Done():
				return
			case <-t.C:
				if foreign := h.in.Foreign(); len(foreign) > 0 {
					cmd := win.CommandLine(foreign[0].PID)
					h.userCmd.Store(&cmd)
					h.cancel()
					return
				}
				if n, shown := h.in.EndStale(); n > 0 {
					// A launcher that the update machinery started late.
					r.logf("ended %d launcher process(es) left over from the launcher's self-update", n)
					h.seen.add(shown...)
				}
				if stray := h.in.OnUserDesktop(); len(stray) > 0 {
					for _, w := range stray {
						win.HideWindow(w.HWND)
					}
					h.seen.add(stray...)
					if !warned {
						warned = true
						r.logf("a window of the hidden launcher appeared on the visible desktop and was hidden again (class %q, title %q)", stray[0].Class, stray[0].Title)
					}
				}
			}
		}
	}()
}

// close ends the hidden launcher and the watch. With a grace period the
// launcher is first asked to close and given that long to do so; the watch
// stays on meanwhile, and the wait ends at once if the user asks for the
// launcher (h.userCmd is then set, and the caller hands over).
func (h *hidden) close(grace time.Duration) {
	if grace > 0 && h.userCmd.Load() == nil && h.in.AskToClose() {
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) && h.userCmd.Load() == nil && h.in.Alive() {
			time.Sleep(200 * time.Millisecond)
		}
	}
	h.cancel()
	h.watch.Wait()
	h.in.Stop(0)
}

// finish shuts the hidden launcher down at the end of a session. If the
// user asked for the launcher at any point, the shutdown included, it is
// started for them and true is returned.
func (r *Runner) finish(inst launcher.Install, h *hidden, grace time.Duration) (handedOver bool) {
	if h.userCmd.Load() == nil {
		h.close(grace)
	}
	if h.userCmd.Load() != nil {
		r.handOver(inst, h)
		return true
	}
	return false
}

// handOver gets out of the user's way: their start of the launcher was
// swallowed by the hidden one, so that one is ended at once and the launcher
// is started for them, with whatever link they started it with.
func (r *Runner) handOver(inst launcher.Install, h *hidden) {
	cmd := ""
	if c := h.userCmd.Load(); c != nil {
		cmd = *c
	}
	h.close(0)
	if err := launcher.StartForUser(inst, false, launcher.UserArgs(cmd)); err != nil {
		r.logf("could not start the launcher for the user: %v", err)
	}
}

// bringUp starts the hidden launcher and waits for its DevTools endpoint.
// A launcher that first wants to update itself is let do so. On errUserAsked
// the launcher is already running for the user.
func (r *Runner) bringUp(ctx context.Context, inst launcher.Install, sn *seen) (*hidden, error) {
	// A launcher with an update of its own to install exits instead of
	// coming up. It is then let update (see launcher.UpdateRun) and started
	// again; a large update can take more than one such round.
	const attempts = 4
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Nothing of an earlier round may be left to swallow this start.
			if n := launcher.KillLeftovers(); n > 0 {
				r.logf("ended %d launcher process(es) left over from the previous round", n)
			}
		}
		port, err := launcher.FreePort()
		if err != nil {
			return nil, err
		}
		in, err := launcher.StartHidden(inst, port)
		if err != nil {
			return nil, fmt.Errorf("start the hidden launcher: %w", err)
		}
		h := &hidden{in: in, base: cdp.Base(port), seen: sn}
		h.ctx, h.cancel = context.WithCancel(ctx)
		r.startWatch(h)

		up, exited := in.WaitDevTools(h.ctx, 3*time.Minute)
		if up {
			return h, nil
		}
		if h.userCmd.Load() != nil {
			r.handOver(inst, h)
			return nil, errUserAsked
		}
		h.close(0)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !exited {
			return nil, errors.New("the hidden launcher did not open its DevTools endpoint (this launcher version may no longer support it)")
		}
		seen := launcher.Describe()
		if n := launcher.KillLeftovers(); n > 0 {
			// The hidden start was passed on to a launcher that the
			// launcher's own update machinery had started late.
			r.logf("the hidden start was taken by a launcher left over from the self-update (%s); ended it and starting again", seen)
			continue
		}
		if launcher.Running() {
			// The hidden start was passed on to a launcher that something
			// else started in the meantime (Windows at login, the user).
			return nil, &daemon.BusyError{Reason: "the launcher was started by something else just now"}
		}
		if attempt == attempts-1 {
			return nil, errors.New("the hidden launcher keeps exiting before it is ready, also after it was given the chance to update itself")
		}
		if attempt == 0 {
			r.logf("the launcher closed itself right after starting, which it does when it has an update of its own to install; letting it update in the tray first")
		} else {
			r.logf("the launcher closed itself again (launcher processes seen: %s); its update does not seem to be finished, so it gets another round in the tray", seen)
		}
		flickers, err := launcher.UpdateRun(ctx, inst, r.logf)
		sn.mu.Lock()
		sn.flickers += flickers
		sn.mu.Unlock()
		if err != nil {
			if errors.Is(err, launcher.ErrUserStarted) {
				return nil, errUserAsked
			}
			return nil, fmt.Errorf("launcher self-update: %w", err)
		}
	}
	return nil, errors.New("the hidden launcher could not be started")
}

// openSession summons the launcher's interface and attaches to it.
func (r *Runner) openSession(ctx context.Context, in *launcher.Instance, base string) (*claim.Session, error) {
	opt := claim.Options{DryRun: r.DryRun, Again: r.Again, DiagDir: r.DiagDir, Logf: r.Logf}
	// Fallback route: have the launcher show a game's store page itself.
	opt.OpenProduct = func(ctx context.Context, o feed.Offer, try int) error {
		links := launcher.StoreLinks(o.PagePath, o.Slug)
		if len(links) == 0 {
			return errors.New("the offer has no store page")
		}
		return in.Summon(links[try%len(links)])
	}
	var lastErr error
	// The interface is created when the launcher is asked to show itself.
	// Ask, wait a while, and ask again if nothing came up.
	for i, wait := range []time.Duration{45 * time.Second, 75 * time.Second, 2 * time.Minute} {
		if err := in.Summon(); err != nil {
			r.logf("could not summon the launcher's interface: %v", err)
		}
		o := opt
		o.UIWait = wait
		s, err := claim.Open(ctx, base, o)
		if err == nil {
			return s, nil
		}
		lastErr = err
		if errors.Is(err, claim.ErrNotSignedIn) || ctx.Err() != nil || !in.Alive() {
			break
		}
		r.logf("the launcher's interface has not come up yet (try %d): %v", i+1, err)
	}
	return nil, lastErr
}

// makeRoom checks that a hidden launcher may run now and, if the user's own
// launcher is idle in the tray, closes it. restore reports that it should be
// put back afterwards. release must be called when the session is over.
func (r *Runner) makeRoom() (inst launcher.Install, restore bool, release func(), err error) {
	inst, err = launcher.Locate(r.LauncherPath)
	if err != nil {
		if errors.Is(err, launcher.ErrNotInstalled) {
			err = fmt.Errorf("%w (%v)", daemon.ErrNotInstalled, err)
		}
		return inst, false, nil, err
	}
	if inst.Gen != launcher.Legacy {
		return inst, false, nil, fmt.Errorf("%w: %s", daemon.ErrUnsupported, unsupportedNote)
	}
	release, ok, lockErr := win.SingleInstance(sessionLock)
	if lockErr != nil {
		return inst, false, nil, lockErr
	}
	if !ok {
		return inst, false, nil, &daemon.BusyError{Reason: "another copy of this program is running a session"}
	}
	fail := func(e error) (launcher.Install, bool, func(), error) {
		release()
		return inst, false, nil, e
	}
	// Never get in the user's way.
	if win.FullscreenActivity() {
		return fail(&daemon.BusyError{Reason: "a full-screen game or presentation is running"})
	}
	if n := launcher.KillLeftovers(); n > 0 {
		r.logf("ended %d hidden launcher(s) left over from an earlier run", n)
	}
	act := launcher.Inspect()
	if act.Running {
		if act.Busy {
			return fail(&daemon.BusyError{Reason: act.Reason})
		}
		if !r.RestartIdleLauncher {
			return fail(&daemon.BusyError{Reason: "the launcher is running in the tray, and the settings say not to restart it"})
		}
		if stopped, why := launcher.StopUserInstance(act); !stopped {
			return fail(&daemon.BusyError{Reason: why})
		}
		r.logf("the launcher was idle in the tray; closed it for a moment so a hidden one can run")
		restore = true
	}
	return inst, restore, release, nil
}

// ClaimAll runs one session for the given offers.
func (r *Runner) ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	results := map[string]claim.Result{}

	inst, restore, release, err := r.makeRoom()
	if err != nil {
		return results, err
	}
	defer release()
	handedOver := false
	defer func() {
		if restore && !handedOver {
			if err := launcher.StartForUser(inst, true, nil); err != nil {
				r.logf("could not put the launcher back in the tray: %v", err)
			} else {
				r.logf("the launcher is back in the tray")
			}
		}
	}()

	// The launcher can go away in the middle of a claim. It is then started
	// again (bringUp lets it install an update of its own first, if that is
	// why it left) and the claim is taken up afresh, a few times at most.
	sn := &seen{}
	err = rounds(offers, maxRestarts, func(pending []feed.Offer, mayRestart bool) (int, error) {
		lostAt, yielded, err := r.session(ctx, inst, sn, pending, mayRestart, results)
		if yielded {
			handedOver = true // the launcher is running for the user now
			return -1, daemon.ErrYielded
		}
		if lostAt >= 0 {
			// Nothing of the launcher that went away may be left to swallow
			// the next start.
			if n := launcher.KillLeftovers(); n > 0 {
				r.logf("ended %d launcher process(es) left over from the launcher that went away", n)
			}
			// As before the first start: never in the user's way. A
			// launcher that runs now is somebody else's (a hidden start
			// would be passed on to it and bring its window up).
			if launcher.Running() || win.FullscreenActivity() {
				r.logf("the launcher is not started again right now (it is running for someone else, or something is full-screen); what is left waits for the next look")
				return -1, err
			}
		}
		return lostAt, err
	})
	var busy *daemon.BusyError
	if len(results) > 0 && errors.As(err, &busy) {
		// "Busy" means nothing was attempted. Here something was, in a
		// round before the launcher had to be started again: what came of
		// it is kept, and the rest waits as it would after "busy".
		r.logf("the launcher could not be started again: %s; what is left waits for the next look", busy.Reason)
		err = nil
	}
	return results, err
}

// session runs one hidden launcher over the given offers and writes what
// came of each into results. lostAt is -1, or (only with mayRestart) the
// index of the offer during which the launcher went away: that offer has no
// result yet, and neither have those behind it. yielded reports that the
// user asked for the launcher, which is then running for them.
func (r *Runner) session(ctx context.Context, inst launcher.Install, sn *seen, offers []feed.Offer, mayRestart bool, results map[string]claim.Result) (lostAt int, yielded bool, err error) {
	lostAt = -1
	started := time.Now()
	h, err := r.bringUp(ctx, inst, sn)
	if err != nil {
		if errors.Is(err, errUserAsked) {
			return -1, true, nil
		}
		return -1, false, err
	}
	defer h.close(0) // whatever happens below, it does not outlive this call
	r.logf("hidden %s is up after %v (DevTools on port %d)", inst.Gen, time.Since(started).Round(time.Second), h.in.Port)

	sess, err := r.openSession(h.ctx, h.in, h.base)
	if err == nil {
		for i, o := range offers {
			if h.ctx.Err() != nil {
				break
			}
			if i > 0 {
				select {
				case <-h.ctx.Done():
				case <-time.After(3 * time.Second):
				}
			}
			r.logf("claiming %s ...", o.Title)
			res := sess.Claim(h.ctx, o)
			if h.ctx.Err() != nil && res.Outcome == claim.Failed && strings.HasPrefix(res.Detail, "interrupted") && !strings.Contains(res.Detail, "after the order button") {
				break // never got anywhere; do not record it as an attempt
			}
			gone := res.Outcome == claim.Failed && sess.Gone() && h.ctx.Err() == nil
			if gone && mayRestart {
				// (It closes itself when it has an update of its own to
				// install, and it can crash.)
				r.logf("%s: %s. Starting the launcher again and taking this up afresh", o.Title, res.Detail)
				if afterAPress(res) {
					// The press stays on record should no later round
					// get to this offer; one that does writes over it.
					results[o.Key()] = res
				}
				lostAt = i
				break
			}
			prev, had := results[o.Key()]
			results[o.Key()] = settle(prev, had, res)
			if gone {
				break // the offers behind it were not reached; they keep their turn
			}
			if res.Outcome == claim.NotSignedIn || res.Outcome == claim.Captcha || res.Outcome == claim.Throttled {
				break // these concern the account: every other offer would end the same way
			}
		}
		sess.Close()
	}

	grace := 8 * time.Second
	if ctx.Err() != nil {
		// Asked to stop (the program is being closed, or a console window
		// was): be quick, so that the launcher is back in the tray in time.
		grace = 0
	}
	if r.finish(inst, h, grace) {
		return -1, true, nil
	}
	if err != nil {
		return -1, false, err
	}
	return lostAt, false, ctx.Err()
}

// LaunchReport is what a trial run of the hidden launcher found.
type LaunchReport struct {
	Install        launcher.Install
	Up             bool
	StartupSeconds float64
	Browser        string
	UserAgent      string
	Targets        []cdp.Target
	TargetsAfter   []cdp.Target
	HiddenWindows  []win.Window
	OnUserDesktop  []win.Window // still showing on the visible desktop at the end
	Leaked         []win.Window // showed on the visible desktop at some point (hidden again at once)
	// UpdateFlickers counts the windows the launcher tried to show while it
	// installed an update of its own, which it can only do in its tray mode
	// on the visible desktop; each was put away within a hundredth of a
	// second or so.
	UpdateFlickers int
	SignedIn       string // "yes", "no", "unknown"
	Pages          []PageSurvey
	Err            string
}

// PageSurvey describes one launcher page: where it is, which buttons it
// shows, and which functions the launcher offers to it. No page text and no
// account details are collected.
type PageSurvey struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Buttons []string `json:"buttons"`
	Bridge  []string `json:"bridge"`
	Err     string   `json:"err,omitempty"`
}

const surveyJS = `(function () {
  function t(el) { return ((el.innerText || el.textContent) || '').replace(/\s+/g, ' ').trim().slice(0, 40); }
  var out = { url: location.origin + location.pathname, title: document.title.slice(0, 80), buttons: [], bridge: [] };
  var bs = document.querySelectorAll('button, [role="button"]');
  for (var i = 0; i < bs.length && out.buttons.length < 25; i++) {
    var r = bs[i].getBoundingClientRect();
    var label = t(bs[i]) || (bs[i].getAttribute('aria-label') || '').slice(0, 40);
    if (r.width > 4 && r.height > 4 && label) out.buttons.push(label);
  }
  try {
    var ue = window.ue;
    if (ue) for (var k in ue) {
      var names = [];
      try { for (var f in ue[k]) names.push(f); } catch (e) {}
      out.bridge.push(k + (names.length ? ': ' + names.slice(0, 30).join(' ') : ''));
      if (out.bridge.length >= 40) break;
    }
  } catch (e) {}
  return out;
})()`

func surveyPages(ctx context.Context, base string) []PageSurvey {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	targets, err := cdp.Targets(lctx, base)
	cancel()
	if err != nil {
		return nil
	}
	var out []PageSurvey
	for _, t := range targets {
		if t.Type != "page" || len(out) >= 6 {
			continue
		}
		var ps PageSurvey
		func() {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			page, err := cdp.OpenPage(pctx, t)
			if err != nil {
				ps.URL, ps.Err = t.URL, err.Error()
				return
			}
			defer page.Close()
			if err := page.Eval(pctx, surveyJS, &ps); err != nil {
				ps.URL, ps.Err = t.URL, err.Error()
			}
		}()
		if i := strings.IndexAny(ps.URL, "?#"); i >= 0 {
			ps.URL = ps.URL[:i]
		}
		out = append(out, ps)
	}
	return out
}

// TrialLaunch starts the hidden launcher, looks at what it exposes, and
// shuts it down again. It claims nothing. It refuses to run while the user's
// own launcher is open.
func (r *Runner) TrialLaunch(ctx context.Context) (rep LaunchReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, restore, release, err := r.makeRoom()
	rep.Install = inst
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer release()
	handedOver := false
	defer func() {
		if restore && !handedOver {
			_ = launcher.StartForUser(inst, true, nil)
		}
	}()

	started := time.Now()
	sn := &seen{}
	defer func() { rep.Leaked, rep.UpdateFlickers = sn.report() }()
	h, err := r.bringUp(ctx, inst, sn)
	if err != nil {
		if errors.Is(err, errUserAsked) {
			handedOver = true
			rep.Err = "the launcher was started by the user; stepped aside"
			return rep
		}
		rep.Err = err.Error()
		return rep
	}
	defer h.close(0)
	in, base := h.in, h.base
	rep.Up = true
	rep.StartupSeconds = time.Since(started).Seconds()

	// Everything from here on ends at once if the user asks for the
	// launcher (h.ctx is then cancelled by the watch).
	pause := func(d time.Duration) {
		select {
		case <-h.ctx.Done():
		case <-time.After(d):
		}
	}
	vctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	if v, err := cdp.Version(vctx, base); err == nil {
		rep.Browser, rep.UserAgent = v.Browser, v.UserAgent
	}
	cancel()
	list := func() []cdp.Target {
		lctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
		defer cancel()
		t, _ := cdp.Targets(lctx, base)
		for i := range t {
			t[i].WebSocketDebuggerURL = ""
		}
		return t
	}
	pause(3 * time.Second)
	rep.Targets = list()

	rep.SignedIn = "unknown"
	if h.ctx.Err() == nil {
		if err := in.Summon(); err != nil {
			r.logf("summon: %v", err)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && h.ctx.Err() == nil {
		pause(2 * time.Second)
		rep.TargetsAfter = list()
		pages, login := 0, false
		for _, t := range rep.TargetsAfter {
			if t.Type == "page" {
				pages++
				if strings.Contains(t.URL, "epicgames.com/id/") {
					login = true
				}
			}
		}
		if pages > 0 {
			if login {
				rep.SignedIn = "no"
			} else {
				rep.SignedIn = "yes"
			}
			break
		}
	}
	if rep.SignedIn == "yes" && h.ctx.Err() == nil {
		// Give the launcher's store page a moment to finish loading.
		pause(8 * time.Second)
		rep.TargetsAfter = list()
	}
	if h.ctx.Err() == nil {
		rep.Pages = surveyPages(h.ctx, base)
		rep.HiddenWindows = in.HiddenWindows()
	}
	rep.OnUserDesktop = in.OnUserDesktop()
	grace := 8 * time.Second
	if ctx.Err() != nil {
		grace = 0 // asked to stop: be quick about it
	}
	if r.finish(inst, h, grace) {
		handedOver = true
		rep.Err = "the launcher was started by the user during the trial; stepped aside"
	}
	if ctx.Err() != nil && rep.Err == "" {
		rep.Err = "interrupted"
	}
	return rep
}
