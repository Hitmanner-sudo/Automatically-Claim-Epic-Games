//go:build windows

// EpicAutoClaim watches Epic's free-games list and claims each giveaway
// through the Epic Games Launcher that is installed and signed in on this
// PC, without showing anything on screen.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/claim"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/config"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/daemon"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/launcher"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/logx"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/runner"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/sched"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/state"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

// version is set at build time.
var version = "dev"

const (
	appName     = "EpicAutoClaim"
	exeName     = "EpicAutoClaim.exe"
	runKey      = `Software\Microsoft\Windows\CurrentVersion\Run`
	mutexName   = `Local\EpicAutoClaim-daemon`
	installLock = `Local\EpicAutoClaim-install`
	stopName    = `Local\EpicAutoClaim-stop`
	dialogName  = "Epic Auto Claim"
)

type paths struct {
	dir, exe, config, state, log, diag string
}

func appPaths() (paths, error) {
	local, err := win.LocalAppData()
	if err != nil {
		return paths{}, err
	}
	dir := filepath.Join(local, appName)
	return paths{
		dir:    dir,
		exe:    filepath.Join(dir, exeName),
		config: filepath.Join(dir, "config.json"),
		state:  filepath.Join(dir, "state.json"),
		log:    filepath.Join(dir, "logs", appName+".log"),
		diag:   filepath.Join(dir, "diagnostics"),
	}, nil
}

// samePath reports whether two paths name the same file, however they are
// spelled (a junction, a mapped drive, a short name).
func samePath(a, b string) bool {
	if sa, err := os.Stat(a); err == nil {
		if sb, err := os.Stat(b); err == nil {
			return os.SameFile(sa, sb)
		}
	}
	ca, err1 := filepath.Abs(a)
	cb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// ---------------------------------------------------------------- autostart

func autostartValue() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(appName)
	return v
}

func setAutostart(exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	// This is what starts the program when the PC is turned on: Windows
	// runs it as the user signs in. ("--login" only says who started it.)
	return k.SetStringValue(appName, `"`+exe+`" run --login`)
}

func removeAutostart() {
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(appName)
		k.Close()
	}
}

// daemonRunning reports whether the background process is alive.
func daemonRunning() bool { return win.LockHeld(mutexName) }

// stopDaemon asks the background process to exit and waits for it. It
// reports whether one was running, and whether it is gone now. (In the
// middle of a claim it first shuts the hidden launcher down, which can take
// most of a minute.)
func stopDaemon() (was, gone bool) { return stopDaemonCtx(context.Background()) }

// stopDaemonCtx is stopDaemon with a way to give up waiting early.
func stopDaemonCtx(ctx context.Context) (was, gone bool) {
	if !daemonRunning() {
		return false, true
	}
	// The request is repeated for as long as something holds the lock: a
	// manual run, or the successor it arranged, may take the old process's
	// place half-way through, and must hear it too. "Gone" means the lock
	// has stayed free for a moment, not that it was free once.
	deadline := time.Now().Add(60 * time.Second)
	free := 0
	for time.Now().Before(deadline) && ctx.Err() == nil && free < 5 {
		if daemonRunning() {
			free = 0
			win.RaiseStopSignal(stopName)
		} else {
			free++
		}
		time.Sleep(200 * time.Millisecond)
	}
	return true, free >= 5
}

// ---------------------------------------------------------------- pieces

type app struct {
	p      paths
	cfgErr error // why the settings file could not be used as written
	cfg    config.Config
	log    *logx.Logger
	st     *state.Store
	out    io.Writer
	// again: a manual run that goes through the checkout also for games
	// the account already has (see claim.Options.Again).
	again bool
}

func country(cfg config.Config) string {
	if cfg.Country != "" {
		return cfg.Country
	}
	if c := win.Country(); c != "" {
		return c
	}
	return "US"
}

func newApp(out io.Writer, mirror bool) (*app, error) {
	p, err := appPaths()
	if err != nil {
		return nil, err
	}
	cfg, cfgErr := config.Load(p.config)
	var m io.Writer
	if mirror {
		m = out
	}
	log, err := logx.Open(p.log, m)
	if err != nil {
		log = logx.Discard(m)
	}
	if cfgErr != nil {
		log.Printf("%v", cfgErr)
	}
	st, err := state.Open(p.state)
	if err != nil {
		return nil, err
	}
	return &app{p: p, cfg: cfg, cfgErr: cfgErr, log: log, st: st, out: out}, nil
}

func (a *app) runner() *runner.Runner {
	r := &runner.Runner{
		LauncherPath:        a.cfg.LauncherPath,
		RestartIdleLauncher: a.cfg.RestartIdleLauncher,
		DryRun:              a.cfg.DryRun,
		Again:               a.again,
		Logf:                a.log.Printf,
	}
	if a.cfg.KeepDiagnostics {
		r.DiagDir = a.p.diag
	}
	return r
}

func (a *app) daemon() *daemon.Daemon {
	d := &daemon.Daemon{
		Feed:       &feed.Client{},
		Claimer:    a.runner(),
		State:      a.st,
		Logf:       a.log.Printf,
		Country:    country(a.cfg),
		CheckEvery: time.Duration(a.cfg.CheckEveryMinutes) * time.Minute,
	}
	if h, m, ok := config.ParseClock(a.cfg.DailyAt); ok {
		d.Slot = daemon.DailySlot(h, m, time.Local)
	}
	return d
}

// howOften says, for people, when the program looks for free games.
func howOften(cfg config.Config) string {
	if h, m, ok := config.ParseClock(cfg.DailyAt); ok {
		return fmt.Sprintf("once a day at about %02d:%02d", h, m)
	}
	switch n := cfg.CheckEveryMinutes; {
	case n <= 0 || n == 60:
		return "every hour"
	case n%60 == 0:
		return fmt.Sprintf("every %d hours", n/60)
	default:
		return fmt.Sprintf("every %d minutes", n)
	}
}

// The background process is started as the user signs in to Windows. It
// holds off this long first: the network is not up yet at that moment, and
// the launcher's own start with Windows is still under way.
const settleAfterSignIn = 2 * time.Minute

func settle() time.Duration {
	if since, ok := win.SignedInSince(); ok {
		if hold := settleAfterSignIn - time.Since(since); hold > 0 {
			return hold
		}
	}
	return 0
}

// fileStamp tells one state of a file from another: its size and the time
// it was last written ("" if it is not there).
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
}

// whatBringsItBack says, after the background process has been stopped,
// when Windows will start it again.
func whatBringsItBack() string {
	name := taskName()
	switch {
	case name != "" && sched.Exists(name):
		return "Windows starts it again within the hour (to remove it for good: EpicAutoClaim.exe uninstall)."
	case autostartValue() != "":
		return "It starts again when you next sign in to Windows (to remove it for good: EpicAutoClaim.exe uninstall)."
	}
	return "Nothing starts it again: it is not installed."
}

// taskSpec is the hourly task as it should be registered for this user:
// what brings the program back if it is not running.
func taskSpec(p paths) (sched.Spec, error) {
	sid, err := sched.CurrentUserSID()
	if err != nil {
		return sched.Spec{}, err
	}
	return sched.Spec{
		Name: sched.TaskName(appName, sid), Exe: p.exe, Args: "run --scheduled", Dir: p.dir,
		UserSID: sid,
	}, nil
}

// registerTask creates (or replaces) the hourly task.
func registerTask(p paths) (sched.Spec, error) {
	spec, err := taskSpec(p)
	if err != nil {
		return spec, err
	}
	return spec, sched.Register(spec)
}

// taskName is the name of this user's hourly task ("" if it cannot be told).
func taskName() string {
	sid, err := sched.CurrentUserSID()
	if err != nil {
		return ""
	}
	return sched.TaskName(appName, sid)
}

// daemonArgs is how the background process is started by hand (after an
// installation, or after a manual run).
func daemonArgs() []string { return []string{"run"} }

// installed reports whether Windows has been told to start the program.
func installed() bool {
	return autostartValue() != "" || (taskName() != "" && sched.Exists(taskName()))
}

// ---------------------------------------------------------------- commands

// cmdRun is the background process. afterPID, when set, is a manual run
// ("once") that stopped the background process and started this one to take
// over as soon as it has ended, however it ends.
//
// It stays for as long as the user is signed in, whoever started it: the
// start-up entry at sign-in, Windows' hourly task (scheduled; the one that
// brings it back if it was ended), or an installation or manual run.
func cmdRun(a *app, scheduled bool, afterPID uint32) int {
	var stop *win.StopSignal
	if afterPID != 0 {
		stop, _ = win.NewStopSignal(stopName)
		if win.WaitExitOrStop(afterPID, stop) {
			return 0 // told to stop while waiting
		}
	}
	var release func()
	for try := 0; ; try++ {
		r, ok, err := win.SingleInstance(mutexName)
		if err != nil {
			a.log.Printf("cannot start: %v", err)
			return 1
		}
		if ok {
			release = r
			break
		}
		// A moment's patience in any case (another command may just be
		// looking at the lock); a successor waits as long as its
		// predecessor may need to wind down.
		if (afterPID == 0 && try >= 3) || try >= 450 {
			return 0 // already running
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer release()
	if afterPID != 0 {
		// Whoever held the lock before has recorded what it did by now;
		// start from that.
		if st, err := state.Open(a.p.state); err == nil {
			a.st = st
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if stop == nil {
		stop, _ = win.NewStopSignal(stopName)
	}
	if stop != nil {
		defer stop.Close()
		go func() {
			stop.Wait()
			a.log.Printf("asked to stop")
			cancel()
		}()
	}

	mode := ""
	if a.cfg.DryRun {
		mode = " (dry run: no orders will be placed)"
	}
	if a.cfgErr != nil {
		a.log.Printf("%v", a.cfgErr)
	}
	d := a.daemon()
	// The program runs for as long as the user is signed in, and reads its
	// settings once, as it starts. So before every round it looks whether
	// the settings file has been changed since; if so it hands over to a
	// fresh copy of itself, which reads them anew and does that round.
	settings := fileStamp(a.p.config)
	holdOff := func() time.Duration {
		if hold := settle(); hold > 0 {
			return hold
		}
		if fileStamp(a.p.config) == settings {
			return 0
		}
		self, err := os.Executable()
		if err == nil {
			err = win.StartDetached(self, append(daemonArgs(), "--after-pid="+strconv.Itoa(os.Getpid())), a.p.dir)
		}
		if err != nil {
			// Carry on with the settings as read; say so, once.
			a.log.Printf("the settings file has changed, but the program could not be started afresh to read it (%v); the changes apply from the next start", err)
			settings = fileStamp(a.p.config)
			return 0
		}
		a.log.Printf("the settings file has changed; starting afresh to read it")
		cancel()
		return time.Minute
	}
	// While it waits the program makes itself as small as it can: it gives
	// its memory back to Windows and drops to the lowest priority for the
	// processor, the disk and memory (Windows' "efficiency mode").
	opt := daemon.RunOptions{Settle: holdOff, Waiting: func(waiting bool) {
		if waiting {
			debug.FreeOSMemory()
		}
		win.Background(waiting)
	}}
	due := d.Due()
	if scheduled {
		keepTaskCurrent(a)
	}
	a.log.Printf("%s %s started%s; country %s; looking %s; next look %s", appName, version, mode, country(a.cfg), howOften(a.cfg), describeDue(due))
	d.Run(ctx, opt)
	a.log.Printf("stopped")
	return 0
}

func describeDue(due time.Time) string {
	if !due.After(time.Now()) {
		return "now"
	}
	return due.Local().Format("Mon 2 Jan 15:04")
}

// keepTaskCurrent re-registers the hourly task when it no longer matches
// this program's location or was registered by an earlier version (which
// had a task for a set time of day, and no start-up entry: that is put
// right as well). Windows has just started the program through that task,
// so it exists; if it cannot be read, it is left alone.
func keepTaskCurrent(a *app) {
	spec, err := taskSpec(a.p)
	if err != nil {
		return
	}
	def := sched.Definition(spec.Name)
	if def == "" {
		return
	}
	// The task carries a marker saying what it was registered for.
	if strings.Contains(def, spec.Marker()) {
		return
	}
	if _, err := registerTask(a.p); err != nil {
		a.log.Printf("Windows' hourly task could not be brought up to date: %v", err)
	} else {
		a.log.Printf("Windows' hourly task was brought up to date")
	}
	// (Only together with an out-of-date task: an entry that is missing
	// next to an up-to-date one was taken away on purpose, by "uninstall"
	// or by the user, and is not put back.)
	if autostartValue() == "" {
		if err := setAutostart(a.p.exe); err == nil {
			a.log.Printf("the program now also starts as you sign in to Windows")
		}
	}
}

func cmdOnce(a *app, args []string) int {
	dry, again := false, false
	for _, arg := range args {
		switch arg {
		case "--dry-run", "-n":
			dry = true
		case "--again":
			again = true
		default:
			// A mistyped "--dry-run" must not turn into a real run.
			fmt.Fprintf(a.out, "Unknown option %q for \"once\"; nothing was done.\n\n%s", arg, usage)
			return 2
		}
	}
	a.again = again
	trial := dry && !a.cfg.DryRun
	if dry {
		a.cfg.DryRun = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ctrl+C winds the run down properly (the hidden launcher is closed and
	// a launcher that was in the tray is put back) instead of cutting it off.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(a.out, "\nInterrupted; winding down ...")
		cancel()
	}()

	wasRunning := daemonRunning()
	// The background process comes back when this run is over, however it
	// ends (even if this window is closed half-way through): its successor
	// is started as soon as the old one has been told to stop, and waits
	// for this process to end.
	// (An installed program is running all the time; one is started here
	// even if it was found stopped.)
	isInstalled := wasRunning || installed()
	successorStarted := false
	startSuccessor := func() {
		if !isInstalled || successorStarted {
			return
		}
		successorStarted = true
		if err := win.StartDetached(a.p.exe, append(daemonArgs(), "--after-pid="+strconv.Itoa(os.Getpid())), a.p.dir); err != nil {
			fmt.Fprintf(a.out, "Could not arrange for the background process to come back (%v); double-click %s afterwards.\n", err, exeName)
		}
	}
	if wasRunning {
		fmt.Fprintln(a.out, "The background process is running; stopping it for this manual run.")
		if _, gone := stopDaemonCtx(ctx); !gone {
			startSuccessor()
			if ctx.Err() == nil {
				fmt.Fprintln(a.out, "It is in the middle of something and has not stopped yet. Nothing was done; try again in a minute.")
			}
			return 1
		}
	}
	// For its duration this run is the background process: no second copy
	// starts next to it, and "stop" reaches it.
	release, ok, err := win.SingleInstance(mutexName)
	if err != nil || !ok {
		startSuccessor()
		fmt.Fprintln(a.out, "Another copy has just started; nothing was done.")
		return 1
	}
	defer release()
	if stop, err := win.NewStopSignal(stopName); err == nil {
		defer stop.Close()
		go func() {
			stop.Wait()
			cancel()
		}()
	}
	startSuccessor()
	if wasRunning {
		// Start from what the background process last wrote.
		if st, err := state.Open(a.p.state); err == nil {
			a.st = st
		}
	}
	if again {
		// A rehearsal of everything that is free right now, whatever has
		// been claimed already: it keeps notes of its own, which are thrown
		// away afterwards, and leaves the real ones as they are.
		notes := filepath.Join(a.p.dir, "rehearsal.json")
		_ = os.Remove(notes)
		st, err := state.Open(notes)
		if err != nil {
			fmt.Fprintf(a.out, "Could not set up the rehearsal (%v); nothing was done.\n", err)
			return 1
		}
		a.st = st
		defer os.Remove(notes)
		if a.cfg.DryRun {
			fmt.Fprintln(a.out, "Rehearsing with everything that is free right now, including what is already in the library; nothing is ordered and nothing is recorded.")
		} else {
			fmt.Fprintln(a.out, "Going through everything that is free right now once more, for real, including what is already in the library.\nFor a game you already have, Epic refuses the order and says so: that answer is what this run is for, and nothing changes.\nA game you do not have yet is claimed as usual. Nothing is recorded.")
		}
	}

	d := a.daemon()
	// A manual run retries offers whose waiting period has not passed.
	before := a.st.All()
	for key, e := range before {
		if !e.Status.Done() {
			_ = a.st.Update(key, func(e *state.Entry) { e.NextAttempt = time.Time{} })
		}
	}
	next := d.Tick(ctx)
	releasedByTrial := false
	for key, cur := range a.st.All() {
		if cur.Status.Done() {
			continue
		}
		old, known := before[key]
		switch {
		case known && cur.LastAttempt.Equal(old.LastAttempt):
			// This run did not get to it (the launcher was in use, the
			// list could not be read): its waiting period stands.
			_ = a.st.Update(key, func(e *state.Entry) { e.NextAttempt = old.NextAttempt })
		case trial && strings.HasPrefix(cur.LastResult, string(claim.DryRun)):
			// A trial that went well must not hold up the real thing: the
			// background process may claim this as soon as it looks. (What
			// the trial ran into, a CAPTCHA for instance, keeps its wait.)
			_ = a.st.Update(key, func(e *state.Entry) { e.NextAttempt = time.Time{} })
			releasedByTrial = true
		}
	}
	if releasedByTrial && ctx.Err() == nil {
		// ...and it looks soon, not at the next regular time.
		_ = a.st.SetSchedule(func(sc *state.Schedule) { sc.NextDue = time.Now().Add(time.Minute) })
	}
	switch {
	case ctx.Err() != nil:
		fmt.Fprintln(a.out, "\nStopped before the round was finished.")
	case trial || dry || (again && a.cfg.DryRun):
		fmt.Fprintln(a.out, "\nTrial finished; nothing was ordered.")
	case again:
		// Say whether the run showed what it was made to show.
		proved, claimed := 0, 0
		for _, e := range a.st.All() {
			switch {
			case e.Status == state.Claimed:
				claimed++
			case strings.Contains(e.LastResult, "Epic says the account has this already") || strings.Contains(e.LastResult, "after the press"):
				proved++
			}
		}
		// (Through the log, so that the file says it too: a console's
		// output is easily lost.)
		fmt.Fprintln(a.out)
		switch {
		case proved > 0 || claimed > 0:
			a.log.Printf("Done. The whole path works on this PC: the order button was pressed and Epic answered (%d time(s) \"you have this already\", %d game(s) claimed).", proved, claimed)
		default:
			a.log.Printf("Done, but no order button was pressed in this run with --again, so it shows nothing about the press: the lines above say why.")
		}
	default:
		fmt.Fprintf(a.out, "\nDone. The background process would check again at %s.\n", next.Local().Format("Mon 2 Jan 15:04"))
	}
	printState(a)
	return 0
}

func cmdCheck(a *app) int {
	c := country(a.cfg)
	snap, err := (&feed.Client{}).Fetch(context.Background(), c, "en-US", time.Now())
	if err != nil {
		fmt.Fprintf(a.out, "Could not read the free-games list: %v\n", err)
		return 1
	}
	if snap.Country != "" && !strings.EqualFold(snap.Country, c) && !(strings.EqualFold(c, "UK") && snap.Country == "GB") {
		fmt.Fprintf(a.out, "Epic has no free-games list for %s; this is the one for %s.\n", c, snap.Country)
		c = snap.Country
	}
	if snap.ClockSkew != 0 {
		fmt.Fprintf(a.out, "This PC's clock differs from Epic's by %v; Epic's time is used to tell what is free.\n", (-snap.ClockSkew).Round(time.Minute))
	}
	fmt.Fprintf(a.out, "Free right now in %s:\n", c)
	if len(snap.FreeNow) == 0 {
		fmt.Fprintln(a.out, "  (nothing)")
	}
	for _, o := range snap.FreeNow {
		note := ""
		if e, ok := a.st.Get(o.Key()); ok && e.Status.Done() {
			note = "  [" + string(e.Status) + "]"
		}
		if o.Placeholder {
			note += "  [holiday mystery game]"
		}
		fmt.Fprintf(a.out, "  %s  (until %s)%s\n", o.Title, o.End.Local().Format("Mon 2 Jan 15:04"), note)
	}
	if len(snap.Upcoming) > 0 {
		fmt.Fprintln(a.out, "Coming up:")
		for _, o := range snap.Upcoming {
			fmt.Fprintf(a.out, "  %s  (from %s)\n", o.Title, o.Start.Local().Format("Mon 2 Jan 15:04"))
		}
	}
	return 0
}

func printState(a *app) {
	all := a.st.All()
	if len(all) == 0 {
		fmt.Fprintln(a.out, "No giveaways recorded yet.")
		return
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return all[keys[i]].FirstSeen.After(all[keys[j]].FirstSeen) })
	fmt.Fprintln(a.out, "Giveaways seen:")
	for _, k := range keys {
		e := all[k]
		line := fmt.Sprintf("  %-9s %s", e.Status, e.Title)
		if !e.Status.Done() && !e.NextAttempt.IsZero() {
			line += fmt.Sprintf("  (attempts: %d, next try %s)", e.Attempts, e.NextAttempt.Local().Format("Mon 15:04"))
		}
		if e.LastResult != "" && e.Status != state.Claimed {
			line += "\n            " + e.LastResult
		}
		fmt.Fprintln(a.out, line)
	}
}

func cmdStatus(a *app) int {
	fmt.Fprintf(a.out, "%s %s\n", appName, version)
	name := taskName()
	hasTask, atSignIn := name != "" && sched.Exists(name), autostartValue() != ""
	switch {
	case hasTask && atSignIn:
		fmt.Fprintf(a.out, "Schedule: starts when you sign in to Windows and looks for free games %s. Windows also starts it again within the hour if it is not running (task %q).\n", howOften(a.cfg), name)
	case atSignIn:
		fmt.Fprintf(a.out, "Schedule: starts when you sign in to Windows and looks for free games %s. (There is no Windows task to start it again if it is ended; run \"install\" to add one.)\n", howOften(a.cfg))
	case hasTask:
		fmt.Fprintf(a.out, "Schedule: Windows starts it within the hour if it is not running (task %q), and it looks for free games %s. (There is no start-up entry; run \"install\" to have it start as you sign in.)\n", name, howOften(a.cfg))
	default:
		fmt.Fprintln(a.out, "Schedule: NOT INSTALLED - nothing starts this program. Double-click it (or run \"install\") to set it up.")
	}
	switch {
	case daemonRunning():
		fmt.Fprintln(a.out, "Background process: running")
	case hasTask:
		fmt.Fprintln(a.out, "Background process: not running at the moment (Windows starts it again within the hour)")
	case atSignIn:
		fmt.Fprintln(a.out, "Background process: not running at the moment (it starts the next time you sign in to Windows)")
	default:
		fmt.Fprintln(a.out, "Background process: not running")
	}
	if sc := a.st.Schedule(); !sc.LastCheck.IsZero() || !sc.NextDue.IsZero() {
		last := "never"
		if !sc.LastCheck.IsZero() {
			last = sc.LastCheck.Local().Format("Mon 2 Jan 15:04")
		}
		fmt.Fprintf(a.out, "Free-games list last read: %s; next look: %s\n", last, describeDue(sc.NextDue))
	}
	fmt.Fprintf(a.out, "Folder: %s\n", a.p.dir)
	if inst, err := launcher.Locate(a.cfg.LauncherPath); err != nil {
		fmt.Fprintf(a.out, "Epic Games Launcher: %v\n", err)
	} else {
		fmt.Fprintf(a.out, "Epic Games Launcher: %s (%s)\n", inst.Exe, inst.Gen)
	}
	fmt.Fprintf(a.out, "Country for the free-games list: %s\n", country(a.cfg))
	if a.cfgErr != nil {
		fmt.Fprintf(a.out, "SETTINGS PROBLEM: %v\n", a.cfgErr)
	}
	if a.cfg.DryRun {
		fmt.Fprintln(a.out, "Dry run is ON: nothing will be ordered.")
	}
	printState(a)
	return 0
}

func describeWindow(w win.Window) string {
	vis := "hidden"
	if w.Visible {
		vis = "VISIBLE"
	}
	return fmt.Sprintf("%s class=%q title=%q %dx%d", vis, w.Class, w.Title, w.Right-w.Left, w.Bottom-w.Top)
}

// cmdDoctor prints what the program can see. With --launch it also starts
// the hidden launcher for a moment to prove that part works on this PC.
func cmdDoctor(a *app, args []string) int {
	launch := false
	for _, arg := range args {
		switch arg {
		case "--launch":
			launch = true
		default:
			fmt.Fprintf(a.out, "Unknown option %q for \"doctor\".\n\n%s", arg, usage)
			return 2
		}
	}
	cmdStatus(a)
	fmt.Fprintln(a.out)
	act := launcher.Inspect()
	switch {
	case !act.Running:
		fmt.Fprintln(a.out, "The launcher is not running right now.")
	case act.Busy:
		fmt.Fprintf(a.out, "The launcher is running and in use: %s\n", act.Reason)
	default:
		fmt.Fprintln(a.out, "The launcher is running idle in the tray.")
	}
	fmt.Fprintf(a.out, "Full-screen app in front: %v; keyboard/mouse idle for %v\n", win.FullscreenActivity(), win.IdleFor().Round(time.Second))
	if since, ok := win.SignedInSince(); ok {
		fmt.Fprintf(a.out, "Signed in to Windows since %s (%v ago)\n", since.Local().Format("Mon 2 Jan 15:04"), time.Since(since).Round(time.Minute))
	} else {
		fmt.Fprintln(a.out, "Signed in to Windows since: Windows does not say (the wait after sign-in is then skipped)")
	}
	if !launch {
		fmt.Fprintln(a.out, "\nRun \"doctor --launch\" to try starting the launcher hidden (nothing is claimed).")
		return 0
	}

	fmt.Fprintln(a.out, "\nStarting the launcher hidden (this can take a minute or two) ...")
	// Ctrl+C ends the trial properly: the hidden launcher is closed and a
	// launcher that was in the tray is put back.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	rep := a.runner().TrialLaunch(ctx)
	if rep.Err != "" {
		for _, w := range rep.Leaked {
			fmt.Fprintf(a.out, "  LEAKED (appeared on your desktop for a moment and was hidden): %s\n", describeWindow(w))
		}
		fmt.Fprintf(a.out, "RESULT: FAILED: %s\n", rep.Err)
		return 1
	}
	fmt.Fprintf(a.out, "Hidden launcher came up in %.0f s.\n", rep.StartupSeconds)
	fmt.Fprintf(a.out, "Embedded browser: %s\n  %s\n", rep.Browser, rep.UserAgent)
	fmt.Fprintf(a.out, "Pages before its interface was summoned: %d\n", len(rep.Targets))
	fmt.Fprintf(a.out, "Pages after: %d\n", len(rep.TargetsAfter))
	for _, t := range rep.TargetsAfter {
		u := t.URL
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		fmt.Fprintf(a.out, "  %-8s %s\n", t.Type, u)
	}
	fmt.Fprintf(a.out, "Signed in: %s\n", rep.SignedIn)
	for _, pg := range rep.Pages {
		fmt.Fprintf(a.out, "  page %s  (%s)\n", pg.URL, pg.Title)
		if pg.Err != "" {
			fmt.Fprintf(a.out, "    could not be read: %s\n", pg.Err)
		}
		if len(pg.Buttons) > 0 {
			fmt.Fprintf(a.out, "    buttons: %s\n", strings.Join(pg.Buttons, " | "))
		}
		for _, b := range pg.Bridge {
			fmt.Fprintf(a.out, "    launcher function group %s\n", b)
		}
	}
	visibleHidden := 0
	for _, w := range rep.HiddenWindows {
		if w.Visible {
			visibleHidden++
			fmt.Fprintf(a.out, "  on the private desktop: %s\n", describeWindow(w))
		}
	}
	fmt.Fprintf(a.out, "Windows on the private desktop: %d (%d shown there)\n", len(rep.HiddenWindows), visibleHidden)
	// Counted over the whole trial, not just at its end: a window that
	// showed up on the visible desktop is hidden again within a fraction of
	// a second, but it must not happen at all.
	fmt.Fprintf(a.out, "Launcher windows on YOUR desktop: %d\n", len(rep.OnUserDesktop)+len(rep.Leaked))
	for _, w := range rep.OnUserDesktop {
		fmt.Fprintf(a.out, "  LEAKED (still showing): %s\n", describeWindow(w))
	}
	for _, w := range rep.Leaked {
		fmt.Fprintf(a.out, "  LEAKED (appeared for a moment and was hidden): %s\n", describeWindow(w))
	}
	if rep.UpdateFlickers > 0 {
		fmt.Fprintf(a.out, "NOTE: the launcher installed an update of its own first, which it can only do in its tray mode on your desktop. It tried to show a window %d time(s); each was put away within about a hundredth of a second.\n", rep.UpdateFlickers)
	}
	ok := rep.Up && len(rep.OnUserDesktop) == 0 && len(rep.Leaked) == 0 && len(rep.TargetsAfter) > 0
	if ok {
		fmt.Fprintln(a.out, "RESULT: OK - the launcher can be started and driven without showing anything.")
		if rep.SignedIn == "no" {
			fmt.Fprintln(a.out, "NOTE: the launcher is not signed in. Open it once, sign in and tick \"Remember me\".")
		}
		return 0
	}
	fmt.Fprintln(a.out, "RESULT: FAILED")
	return 1
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".new." + strconv.Itoa(os.Getpid())
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	var last error
	for i := 0; i < 40; i++ {
		if last = os.Rename(tmp, dst); last == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond) // the old copy may still be exiting
	}
	os.Remove(tmp)
	return last
}

// cmdInstall copies the program to its own folder, makes it start with
// Windows and starts it. interactive adds a confirmation dialog: this is
// the one time the program shows anything.
func cmdInstall(interactive bool, out io.Writer) int {
	p, pathErr := appPaths()
	stoppedOld := false
	// One installation at a time (a double-click is easily made twice).
	// The lock is let go before any dialog is shown.
	var installing func()
	releaseInstall := func() {
		if installing != nil {
			installing()
			installing = nil
		}
	}
	defer releaseInstall()
	fail := func(err error) int {
		// A failed upgrade must not leave the PC without the copy that was
		// installed and running before.
		if _, statErr := os.Stat(p.exe); stoppedOld && statErr == nil {
			if daemonRunning() {
				// Told to stop and still winding down: its successor
				// waits for this installer, and then for it, to end.
				_ = win.StartDetached(p.exe, append(daemonArgs(), "--after-pid="+strconv.Itoa(os.Getpid())), p.dir)
			} else {
				_ = win.StartDetached(p.exe, daemonArgs(), p.dir)
			}
		}
		releaseInstall()
		fmt.Fprintf(out, "Install failed: %v\n", err)
		if interactive {
			win.MessageBox(dialogName, "Could not install:\n\n"+err.Error(), true)
		}
		return 1
	}
	if pathErr != nil {
		return fail(pathErr)
	}
	for try := 0; ; try++ {
		r, ok, err := win.SingleInstance(installLock)
		if err == nil && ok {
			installing = r
			break
		}
		if try >= 150 {
			return fail(fmt.Errorf("another installation is still running"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	// The background process is stopped in any case and started afresh
	// below: that is how a new version, or changed settings, take effect.
	was, gone := stopDaemon()
	stoppedOld = was
	if !gone {
		return fail(fmt.Errorf("the copy that is already installed is busy and has not stopped yet; try again in a minute"))
	}
	if !samePath(self, p.exe) {
		if err := copyFile(self, p.exe); err != nil {
			return fail(err)
		}
	}
	if err := config.EnsureFile(p.config); err != nil {
		return fail(err)
	}
	cfg, _ := config.Load(p.config)
	// Two things have Windows start the program: an entry among the
	// programs that start as the user signs in, and a task that starts it
	// every hour (which does nothing while it is running, and brings it
	// back if it was ended). Either is enough to count as installed.
	startErr := setAutostart(p.exe)
	_, taskErr := registerTask(p)
	if startErr != nil && taskErr != nil {
		return fail(fmt.Errorf("Windows took neither the start-up entry (%v) nor the hourly task (%v)", startErr, taskErr))
	}
	how := fmt.Sprintf("It starts when you sign in to Windows, looks for Epic's free games %s (and within minutes of a new giveaway starting), and claims each one through your Epic Games Launcher without showing anything.\n\n", howOften(cfg))
	switch {
	case taskErr != nil:
		how += fmt.Sprintf("(Windows did not accept the hourly task that would start it again if it were ended: %v. It still starts every time you sign in.)\n\n", taskErr)
	case startErr != nil:
		how += fmt.Sprintf("(Windows did not accept the start-up entry: %v. Its hourly task starts the program within the hour instead.)\n\n", startErr)
	}
	if !daemonRunning() {
		if err := win.StartDetached(p.exe, daemonArgs(), p.dir); err != nil {
			return fail(err)
		}
		// Not done before it is up: a second installer (the double-click
		// made twice) must find it running, or it would try to replace a
		// program file that is in use.
		for i := 0; i < 50 && !daemonRunning(); i++ {
			time.Sleep(200 * time.Millisecond)
		}
	}

	msg := "Installed.\n\n" + how
	if inst, err := launcher.Locate(""); err != nil {
		msg += "WARNING: the Epic Games Launcher was not found on this PC. Install it and sign in; this program will pick it up by itself.\n\n"
	} else {
		msg += "Launcher found: " + inst.Exe + "\n\n"
	}
	msg += "One thing to check: the launcher must be signed in with \"Remember me\" ticked.\n\n" +
		"Settings, log and details: " + p.dir
	releaseInstall()
	fmt.Fprintln(out, msg)
	if interactive {
		win.MessageBox(dialogName, msg, false)
	}
	return 0
}

func cmdUninstall(out io.Writer) int {
	p, err := appPaths()
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	removeAutostart()
	if name := taskName(); name != "" {
		if err := sched.Remove(name); err != nil {
			fmt.Fprintf(out, "The task %q could not be removed (%v); delete it in Windows' Task Scheduler.\n", name, err)
		}
	}
	if _, gone := stopDaemon(); !gone {
		fmt.Fprintf(out, "Removed from Windows' start-up and schedule. The background process is finishing something and has not stopped yet; it will not start again.\nYou can delete the folder %s once it is gone.\n", p.dir)
		return 0
	}
	if self, err := os.Executable(); err == nil && !samePath(self, p.exe) {
		_ = os.Remove(p.exe)
	}
	fmt.Fprintf(out, "Stopped and removed from Windows' start-up and schedule.\nYou can delete the folder %s to remove the log and settings.\n", p.dir)
	return 0
}

const usage = `EpicAutoClaim - claims Epic's free games through your Epic Games Launcher, invisibly.

  EpicAutoClaim.exe              install, or repair the installation
  EpicAutoClaim.exe status       what it has done and what it is waiting for
  EpicAutoClaim.exe check        list what is free right now (does not claim)
  EpicAutoClaim.exe once         do one round now instead of waiting
  EpicAutoClaim.exe once --dry-run   same, but stop just before placing the order
  EpicAutoClaim.exe once --dry-run --again   rehearse also with games it has dealt with already
  EpicAutoClaim.exe once --again     prove the whole path: order this week's games again for real
                                 (Epic refuses what you already have; nothing changes)
  EpicAutoClaim.exe doctor       check the setup; add --launch to try the hidden launcher
  EpicAutoClaim.exe stop         stop the background process (Windows starts it again within the hour, or when you next sign in)
  EpicAutoClaim.exe uninstall    stop it and remove it from Windows' start-up and schedule
`

func main() {
	args := os.Args[1:]

	// The background process: no console, no windows, only the log file.
	if len(args) > 0 && args[0] == "run" {
		// It has next to nothing to compute, so it is told to keep its own
		// memory small rather than fast.
		debug.SetGCPercent(25)
		debug.SetMemoryLimit(48 << 20)
		a, err := newApp(io.Discard, false)
		if err != nil {
			os.Exit(1)
		}
		scheduled := false
		var afterPID uint32
		for _, arg := range args[1:] {
			if arg == "--scheduled" {
				scheduled = true
			}
			if v, ok := strings.CutPrefix(arg, "--after-pid="); ok {
				if n, err := strconv.ParseUint(v, 10, 32); err == nil {
					afterPID = uint32(n)
				}
			}
		}
		code := cmdRun(a, scheduled, afterPID)
		a.log.Close()
		os.Exit(code)
	}

	var out io.Writer = io.Discard
	if f := win.ConsoleOut(); f != nil {
		out = f
	}

	// Double-click: install, or bring an existing installation up to date.
	if len(args) == 0 {
		os.Exit(cmdInstall(true, out))
	}

	switch args[0] {
	case "install":
		os.Exit(cmdInstall(false, out))
	case "uninstall":
		os.Exit(cmdUninstall(out))
	case "stop":
		switch was, gone := stopDaemon(); {
		case !was:
			fmt.Fprintln(out, "It was not running.")
		case gone:
			fmt.Fprintln(out, "Stopped. "+whatBringsItBack())
		default:
			fmt.Fprintln(out, "Asked to stop; it is finishing what it was doing and will exit shortly. "+whatBringsItBack())
		}
		os.Exit(0)
	case "version", "--version", "-v":
		fmt.Fprintln(out, version)
		os.Exit(0)
	case "help", "--help", "-h", "/?":
		fmt.Fprint(out, usage)
		os.Exit(0)
	}

	a, err := newApp(out, args[0] == "once" || args[0] == "doctor")
	if err != nil {
		fmt.Fprintln(out, err)
		os.Exit(1)
	}
	code := 2
	switch args[0] {
	case "once":
		code = cmdOnce(a, args[1:])
	case "check":
		code = cmdCheck(a)
	case "status":
		code = cmdStatus(a)
	case "doctor":
		code = cmdDoctor(a, args[1:])
	default:
		fmt.Fprintf(out, "Unknown command %q.\n\n%s", args[0], usage)
	}
	a.log.Close()
	os.Exit(code)
}
