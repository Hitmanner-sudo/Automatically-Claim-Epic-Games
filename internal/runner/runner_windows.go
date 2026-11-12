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

// Runner implements daemon.Claimer.
type Runner struct {
	LauncherPath        string
	RestartIdleLauncher bool
	DryRun              bool
	DiagDir             string
	Logf                func(format string, args ...any)

	mu sync.Mutex // one session at a time
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// startHidden brings up the hidden launcher and returns it together with
// the base address of its DevTools endpoint.
func (r *Runner) startHidden(ctx context.Context, inst launcher.Install) (*launcher.Instance, string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		port, err := launcher.FreePort()
		if err != nil {
			return nil, "", err
		}
		in, err := launcher.StartHidden(inst, port)
		if err != nil {
			return nil, "", fmt.Errorf("start the hidden launcher: %w", err)
		}
		up, exited := in.WaitDevTools(ctx, 3*time.Minute)
		if up {
			return in, cdp.Base(port), nil
		}
		in.Stop(0)
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		if exited && attempt == 0 && inst.Gen == launcher.Legacy {
			r.logf("the launcher closed itself right after starting, which it does when it has an update of its own to install; letting it update in the tray first")
			if err := launcher.UpdateRun(ctx, inst, r.logf); err != nil {
				return nil, "", fmt.Errorf("launcher self-update: %w", err)
			}
			continue
		}
		if exited {
			return nil, "", errors.New("the hidden launcher exited before it was ready")
		}
		return nil, "", errors.New("the hidden launcher did not open its DevTools endpoint (this launcher version may no longer support it)")
	}
	return nil, "", errors.New("the hidden launcher could not be started")
}

// openSession summons the launcher's interface and attaches to it.
func (r *Runner) openSession(ctx context.Context, in *launcher.Instance, base string) (*claim.Session, error) {
	opt := claim.Options{DryRun: r.DryRun, DiagDir: r.DiagDir, Logf: r.Logf}
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

// ClaimAll runs one session for the given offers.
func (r *Runner) ClaimAll(ctx context.Context, offers []feed.Offer) (map[string]claim.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	results := map[string]claim.Result{}

	inst, err := launcher.Locate(r.LauncherPath)
	if err != nil {
		if errors.Is(err, launcher.ErrNotInstalled) {
			return results, fmt.Errorf("%w (%v)", daemon.ErrNotInstalled, err)
		}
		return results, err
	}

	// Never get in the user's way.
	if win.FullscreenActivity() {
		return results, &daemon.BusyError{Reason: "a full-screen game or presentation is running"}
	}
	act := launcher.Inspect()
	restore := false
	if act.Running {
		if act.Busy {
			return results, &daemon.BusyError{Reason: act.Reason}
		}
		if !r.RestartIdleLauncher {
			return results, &daemon.BusyError{Reason: "the launcher is running in the tray, and the settings say not to restart it"}
		}
		r.logf("the launcher is idle in the tray; closing it for a moment so a hidden one can run")
		launcher.StopUserInstance(act)
		restore = true
	}
	yielded := false
	defer func() {
		if restore && !yielded {
			if err := launcher.StartForUser(inst, true, nil); err != nil {
				r.logf("could not put the launcher back in the tray: %v", err)
			} else {
				r.logf("the launcher is back in the tray")
			}
		}
	}()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	started := time.Now()
	in, base, err := r.startHidden(sctx, inst)
	if err != nil {
		return results, err
	}
	defer in.Stop(8 * time.Second)
	r.logf("hidden %s is up after %v (DevTools on port %d)", inst.Gen, time.Since(started).Round(time.Second), in.Port)

	// Watch for the user asking for the launcher while it runs hidden, and
	// make sure nothing of the hidden one ever shows on their desktop.
	var userCommand atomic.Pointer[string]
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		warned := false
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				if foreign := in.Foreign(); len(foreign) > 0 {
					cmd := win.CommandLine(foreign[0].PID)
					userCommand.Store(&cmd)
					cancel()
					return
				}
				if stray := in.OnUserDesktop(); len(stray) > 0 {
					for _, w := range stray {
						win.HideWindow(w.HWND)
					}
					if !warned {
						warned = true
						r.logf("a window of the hidden launcher appeared on the visible desktop and was hidden again (class %q, title %q)", stray[0].Class, stray[0].Title)
					}
				}
			}
		}
	}()

	sess, err := r.openSession(sctx, in, base)
	if err == nil {
		for i, o := range offers {
			if sctx.Err() != nil {
				break
			}
			if i > 0 {
				select {
				case <-sctx.Done():
				case <-time.After(3 * time.Second):
				}
			}
			r.logf("claiming %s ...", o.Title)
			res := sess.Claim(sctx, o)
			if sctx.Err() != nil && res.Outcome == claim.Failed && strings.HasPrefix(res.Detail, "interrupted") && !strings.Contains(res.Detail, "after the order button") {
				break // never got anywhere; do not record it as an attempt
			}
			results[o.Key()] = res
			if res.Outcome == claim.NotSignedIn {
				break // the same would happen for every other offer
			}
		}
		sess.Close()
	}
	cancel()
	watch.Wait()

	if cmd := userCommand.Load(); cmd != nil {
		// The user's start was swallowed by the hidden launcher. Get out of
		// the way at once and start the launcher for them, with whatever
		// link they started it with.
		yielded = true
		in.Stop(0)
		args := launcher.UserArgs(*cmd)
		if err := launcher.StartForUser(inst, false, args); err != nil {
			r.logf("could not start the launcher for the user: %v", err)
		}
		return results, daemon.ErrYielded
	}
	if err != nil {
		return results, err
	}
	return results, ctx.Err()
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
	OnUserDesktop  []win.Window
	SignedIn       string // "yes", "no", "unknown"
	Err            string
}

// TrialLaunch starts the hidden launcher, looks at what it exposes, and
// shuts it down again. It claims nothing. It refuses to run while the user's
// own launcher is open.
func (r *Runner) TrialLaunch(ctx context.Context) LaunchReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	var rep LaunchReport
	inst, err := launcher.Locate(r.LauncherPath)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	rep.Install = inst
	act := launcher.Inspect()
	restore := false
	if act.Running {
		if act.Busy {
			rep.Err = "postponed: " + act.Reason
			return rep
		}
		if !r.RestartIdleLauncher {
			rep.Err = "the launcher is running; close it first"
			return rep
		}
		r.logf("the launcher is idle in the tray; closing it for the trial")
		launcher.StopUserInstance(act)
		restore = true
	}
	defer func() {
		if restore {
			_ = launcher.StartForUser(inst, true, nil)
		}
	}()

	started := time.Now()
	in, base, err := r.startHidden(ctx, inst)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer in.Stop(8 * time.Second)
	rep.Up = true
	rep.StartupSeconds = time.Since(started).Seconds()

	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if v, err := cdp.Version(vctx, base); err == nil {
		rep.Browser, rep.UserAgent = v.Browser, v.UserAgent
	}
	cancel()
	list := func() []cdp.Target {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		t, _ := cdp.Targets(lctx, base)
		for i := range t {
			t[i].WebSocketDebuggerURL = ""
		}
		return t
	}
	time.Sleep(3 * time.Second)
	rep.Targets = list()

	if err := in.Summon(); err != nil {
		r.logf("summon: %v", err)
	}
	rep.SignedIn = "unknown"
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(2 * time.Second)
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
	rep.HiddenWindows = in.HiddenWindows()
	rep.OnUserDesktop = in.OnUserDesktop()
	return rep
}
