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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/config"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/daemon"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/feed"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/launcher"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/logx"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/runner"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/state"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

// version is set at build time.
var version = "dev"

const (
	appName    = "EpicAutoClaim"
	exeName    = "EpicAutoClaim.exe"
	runKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	mutexName  = `Local\EpicAutoClaim-daemon`
	stopName   = `Local\EpicAutoClaim-stop`
	dialogName = "Epic Auto Claim"
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

func samePath(a, b string) bool {
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
	// "--login" tells the background process it was started by Windows at
	// sign-in, when the PC is busy and the launcher's own autostart is
	// still under way.
	return k.SetStringValue(appName, `"`+exe+`" run --login`)
}

func removeAutostart() {
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(appName)
		k.Close()
	}
}

// daemonRunning reports whether the background process is alive.
func daemonRunning() bool {
	release, ok, err := win.SingleInstance(mutexName)
	if err != nil {
		return false
	}
	if ok {
		release()
		return false
	}
	return true
}

func stopDaemon() bool {
	if !daemonRunning() {
		return false
	}
	win.RaiseStopSignal(stopName)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && daemonRunning() {
		time.Sleep(200 * time.Millisecond)
	}
	return true
}

// ---------------------------------------------------------------- pieces

type app struct {
	p   paths
	cfg config.Config
	log *logx.Logger
	st  *state.Store
	out io.Writer
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
	return &app{p: p, cfg: cfg, log: log, st: st, out: out}, nil
}

func (a *app) runner() *runner.Runner {
	r := &runner.Runner{
		LauncherPath:        a.cfg.LauncherPath,
		RestartIdleLauncher: a.cfg.RestartIdleLauncher,
		DryRun:              a.cfg.DryRun,
		Logf:                a.log.Printf,
	}
	if a.cfg.KeepDiagnostics {
		r.DiagDir = a.p.diag
	}
	return r
}

func (a *app) daemon() *daemon.Daemon {
	return &daemon.Daemon{
		Feed:       &feed.Client{},
		Claimer:    a.runner(),
		State:      a.st,
		Logf:       a.log.Printf,
		Country:    country(a.cfg),
		CheckEvery: time.Duration(a.cfg.CheckEveryMinutes) * time.Minute,
	}
}

// ---------------------------------------------------------------- commands

func cmdRun(a *app, atLogin bool) int {
	release, ok, err := win.SingleInstance(mutexName)
	if err != nil {
		a.log.Printf("cannot start: %v", err)
		return 1
	}
	if !ok {
		return 0 // already running
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if stop, err := win.NewStopSignal(stopName); err == nil {
		defer stop.Close()
		go func() {
			stop.Wait()
			a.log.Printf("asked to stop")
			cancel()
		}()
	}

	// Right after sign-in the PC (and the launcher's own autostart) is
	// busy; wait for things to settle. (Uptime is no guide: with Windows'
	// fast start-up it keeps counting across shutdowns.)
	delay := 15 * time.Second
	if atLogin {
		delay = 4 * time.Minute
	}
	mode := ""
	if a.cfg.DryRun {
		mode = " (dry run: no orders will be placed)"
	}
	a.log.Printf("%s %s started%s; country %s; first check in %v", appName, version, mode, country(a.cfg), delay)
	a.daemon().Run(ctx, delay)
	a.log.Printf("stopped")
	return 0
}

func cmdOnce(a *app, args []string) int {
	trial := false
	for _, arg := range args {
		if (arg == "--dry-run" || arg == "-n") && !a.cfg.DryRun {
			a.cfg.DryRun = true
			trial = true
		}
	}
	if daemonRunning() {
		fmt.Fprintln(a.out, "The background process is running; stopping it for this manual run.")
		stopDaemon()
		defer func() { _ = win.StartDetached(a.p.exe, []string{"run"}, a.p.dir) }()
	}
	d := a.daemon()
	// A manual run retries offers whose waiting period has not passed.
	for key, e := range a.st.All() {
		if !e.Status.Done() {
			_ = a.st.Update(key, func(e *state.Entry) { e.NextAttempt = time.Time{} })
		}
	}
	next := d.Tick(context.Background())
	if trial {
		// A one-off trial must not hold up the real thing: the background
		// process may claim these as soon as it looks.
		for key, e := range a.st.All() {
			if !e.Status.Done() {
				_ = a.st.Update(key, func(e *state.Entry) { e.NextAttempt = time.Time{} })
			}
		}
		fmt.Fprintln(a.out, "\nTrial finished; nothing was ordered.")
	} else {
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
	fmt.Fprintf(a.out, "Background process: %s\n", map[bool]string{true: "running", false: "not running"}[daemonRunning()])
	if v := autostartValue(); v != "" {
		fmt.Fprintf(a.out, "Starts with Windows: yes (%s)\n", v)
	} else {
		fmt.Fprintln(a.out, "Starts with Windows: no")
	}
	fmt.Fprintf(a.out, "Folder: %s\n", a.p.dir)
	if inst, err := launcher.Locate(a.cfg.LauncherPath); err != nil {
		fmt.Fprintf(a.out, "Epic Games Launcher: %v\n", err)
	} else {
		fmt.Fprintf(a.out, "Epic Games Launcher: %s (%s)\n", inst.Exe, inst.Gen)
	}
	fmt.Fprintf(a.out, "Country for the free-games list: %s\n", country(a.cfg))
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
		if arg == "--launch" {
			launch = true
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
	if !launch {
		fmt.Fprintln(a.out, "\nRun \"doctor --launch\" to try starting the launcher hidden (nothing is claimed).")
		return 0
	}

	fmt.Fprintln(a.out, "\nStarting the launcher hidden (this can take a minute or two) ...")
	rep := a.runner().TrialLaunch(context.Background())
	if rep.Err != "" {
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
	fmt.Fprintf(a.out, "Launcher windows on YOUR desktop: %d\n", len(rep.OnUserDesktop))
	for _, w := range rep.OnUserDesktop {
		fmt.Fprintf(a.out, "  LEAKED: %s\n", describeWindow(w))
	}
	ok := rep.Up && len(rep.OnUserDesktop) == 0 && len(rep.TargetsAfter) > 0
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
	tmp := dst + ".new"
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
	fail := func(err error) int {
		fmt.Fprintf(out, "Install failed: %v\n", err)
		if interactive {
			win.MessageBox(dialogName, "Could not install:\n\n"+err.Error(), true)
		}
		return 1
	}
	p, err := appPaths()
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	if !samePath(self, p.exe) {
		stopDaemon()
		if err := copyFile(self, p.exe); err != nil {
			return fail(err)
		}
	}
	if err := config.EnsureFile(p.config); err != nil {
		return fail(err)
	}
	if err := setAutostart(p.exe); err != nil {
		return fail(err)
	}
	if !daemonRunning() {
		if err := win.StartDetached(p.exe, []string{"run"}, p.dir); err != nil {
			return fail(err)
		}
	}

	msg := "Installed and running in the background.\n\n" +
		"It starts with Windows, watches Epic's free games, and claims each one through your Epic Games Launcher without showing anything.\n\n"
	if inst, err := launcher.Locate(""); err != nil {
		msg += "WARNING: the Epic Games Launcher was not found on this PC. Install it and sign in; this program will pick it up by itself.\n\n"
	} else {
		msg += "Launcher found: " + inst.Exe + "\n\n"
	}
	msg += "One thing to check: the launcher must be signed in with \"Remember me\" ticked.\n\n" +
		"Settings, log and details: " + p.dir
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
	stopDaemon()
	if self, err := os.Executable(); err == nil && !samePath(self, p.exe) {
		_ = os.Remove(p.exe)
	}
	fmt.Fprintf(out, "Stopped and removed from Windows start-up.\nYou can delete the folder %s to remove the log and settings.\n", p.dir)
	return 0
}

const usage = `EpicAutoClaim - claims Epic's free games through your Epic Games Launcher, invisibly.

  EpicAutoClaim.exe              install (first time) or show status
  EpicAutoClaim.exe status       what it has done and what it is waiting for
  EpicAutoClaim.exe check        list what is free right now (does not claim)
  EpicAutoClaim.exe once         do one round now instead of waiting
  EpicAutoClaim.exe once --dry-run   same, but stop just before placing the order
  EpicAutoClaim.exe doctor       check the setup; add --launch to try the hidden launcher
  EpicAutoClaim.exe stop         stop the background process (it starts again at next login)
  EpicAutoClaim.exe uninstall    stop it and remove it from Windows start-up
`

func main() {
	args := os.Args[1:]

	// The background process: no console, no windows, only the log file.
	if len(args) > 0 && args[0] == "run" {
		a, err := newApp(io.Discard, false)
		if err != nil {
			os.Exit(1)
		}
		atLogin := false
		for _, arg := range args[1:] {
			if arg == "--login" {
				atLogin = true
			}
		}
		code := cmdRun(a, atLogin)
		a.log.Close()
		os.Exit(code)
	}

	var out io.Writer = io.Discard
	if f := win.ConsoleOut(); f != nil {
		out = f
	}

	// Double-click.
	if len(args) == 0 {
		p, err := appPaths()
		self, _ := os.Executable()
		if err == nil && samePath(self, p.exe) && autostartValue() != "" {
			if !daemonRunning() {
				_ = win.StartDetached(p.exe, []string{"run"}, p.dir)
			}
			win.MessageBox(dialogName, "Already installed and running in the background.\n\nLog and settings: "+p.dir, false)
			os.Exit(0)
		}
		os.Exit(cmdInstall(true, out))
	}

	switch args[0] {
	case "install":
		os.Exit(cmdInstall(false, out))
	case "uninstall":
		os.Exit(cmdUninstall(out))
	case "stop":
		if stopDaemon() {
			fmt.Fprintln(out, "Stopped.")
		} else {
			fmt.Fprintln(out, "It was not running.")
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
