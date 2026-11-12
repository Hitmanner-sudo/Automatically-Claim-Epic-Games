//go:build windows

package launcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

// Install describes where the launcher lives on this PC.
type Install struct {
	Exe string // full path of EpicGamesLauncher.exe
	Dir string // its folder, used as the working directory
	Gen Generation
}

// ErrUserStarted is returned when the user started the launcher while this
// program had it running for a self-update. The launcher is then left
// running for them.
var ErrUserStarted = errors.New("the user started the launcher")

// ErrNotInstalled is returned when the launcher cannot be found.
var ErrNotInstalled = errors.New("the Epic Games Launcher was not found on this PC")

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func regString(root registry.Key, path, name string, access uint32) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE|access)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

var bothViews = []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY}

// installRoots returns the folders the launcher's installer recorded.
func installRoots() []string {
	var out []string
	for _, view := range bothViews {
		if dir := regString(registry.LOCAL_MACHINE, `SOFTWARE\EpicGames\Unreal Engine`, "INSTALLDIR", view); dir != "" {
			out = append(out, dir)
		}
	}
	const base = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	for _, view := range bothViews {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, base, registry.ENUMERATE_SUB_KEYS|view)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			if regString(registry.LOCAL_MACHINE, base+`\`+n, "DisplayName", view) != "Epic Games Launcher" {
				continue
			}
			if loc := regString(registry.LOCAL_MACHINE, base+`\`+n, "InstallLocation", view); loc != "" {
				out = append(out, loc)
			}
		}
	}
	return out
}

// Locate finds the launcher. override, when set, is the path from the
// settings file and must point at EpicGamesLauncher.exe.
func Locate(override string) (Install, error) {
	if override = strings.TrimSpace(override); override != "" {
		if !isLauncherExe(override) || !fileExists(override) {
			return Install{}, fmt.Errorf("launcherPath %q is not an existing %s", override, ExeName)
		}
		return Install{Exe: override, Dir: filepath.Dir(override), Gen: generationOf(override)}, nil
	}

	var candidates []string
	add := func(p string) {
		if p != "" {
			candidates = append(candidates, strings.ReplaceAll(p, "/", `\`))
		}
	}

	// 1. A launcher that is running right now is the most certain answer.
	if procs, err := win.Processes(); err == nil {
		for _, p := range procs {
			if isLauncher(p) {
				add(win.ImagePath(p.PID))
			}
		}
	}
	// 2. The com.epicgames.launcher:// link handler the installer registers.
	add(exeFromCommand(regString(registry.CLASSES_ROOT, `com.epicgames.launcher\shell\open\command`, "", 0)))
	// 3. The "run when my computer starts" entry.
	add(exeFromCommand(regString(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, "EpicGamesLauncher", 0)))
	// 4. What Epic Online Services recorded.
	add(regString(registry.CURRENT_USER, `Software\Epic Games\EOS`, "ModSdkCommand", 0))
	// 5. The installer's records.
	for _, root := range installRoots() {
		for _, c := range exeUnder(root) {
			add(c)
		}
	}
	// 6. The default locations (64-bit folder since launcher 20, x86 before).
	for _, env := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
		if base := os.Getenv(env); base != "" {
			for _, c := range exeUnder(base + `\Epic Games`) {
				add(c)
			}
		}
	}
	// 7. The new-generation launcher installs per user.
	if local, err := win.LocalAppData(); err == nil {
		add(local + `\Epic Games\Launcher\` + ExeName)
	}

	for _, c := range candidates {
		if isLauncherExe(c) && fileExists(c) {
			return Install{Exe: c, Dir: filepath.Dir(c), Gen: generationOf(c)}, nil
		}
	}
	return Install{}, ErrNotInstalled
}

// manifestDir is where the launcher records the games it has installed.
func manifestDir() string {
	for _, view := range bothViews {
		if p := regString(registry.LOCAL_MACHINE, `SOFTWARE\Epic Games\EpicGamesLauncher`, "AppDataPath", view); p != "" {
			return filepath.Join(p, "Manifests")
		}
	}
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, `Epic\EpicGamesLauncher\Data\Manifests`)
}

// Activity is a snapshot of what the launcher is doing for the user.
type Activity struct {
	// Running is true when a launcher process exists.
	Running bool
	// Busy is true when the launcher must not be disturbed. Reason says why.
	Busy   bool
	Reason string
	// PIDs are the launcher's main processes.
	PIDs []uint32
}

// helperNames are the launcher's own child processes.
var helperNames = map[string]bool{
	"epicwebhelper.exe":                     true,
	"unrealcefsubprocess.exe":               true,
	"eosoverlayrenderer-win64-shipping.exe": true,
	"msedgewebview2.exe":                    true,
}

// Inspect reports whether the launcher is running and whether the user (or
// a download) is using it.
func Inspect() Activity {
	var a Activity
	procs, err := win.Processes()
	if err != nil {
		// Unknown is treated as busy: never disturb what cannot be seen.
		return Activity{Running: true, Busy: true, Reason: "could not list processes: " + err.Error()}
	}
	tree := map[uint32]bool{}
	var transfer []uint32
	for _, p := range procs {
		if isLauncher(p) {
			a.PIDs = append(a.PIDs, p.PID)
			tree[p.PID] = true
			transfer = append(transfer, p.PID)
		}
		// Since launcher 20 downloads run in this service-side helper.
		if strings.EqualFold(p.Name, "EpicOnlineServicesHost.exe") {
			transfer = append(transfer, p.PID)
		}
	}
	if len(a.PIDs) == 0 {
		return a
	}
	a.Running = true
	for _, p := range procs {
		if tree[p.PPID] && helperNames[strings.ToLower(p.Name)] {
			tree[p.PID] = true
		}
	}

	// An open launcher window means the user is looking at it (or has it
	// parked on the taskbar).
	for _, w := range win.Windows() {
		if tree[w.PID] && w.Visible && (w.Iconic || (w.Right-w.Left >= 200 && w.Bottom-w.Top >= 150)) {
			a.Busy, a.Reason = true, "the launcher window is open"
			return a
		}
	}

	// A game installed through the launcher is running.
	if dirs := gameDirsFromManifests(manifestDir()); len(dirs) > 0 {
		for _, p := range procs {
			if tree[p.PID] || p.PID <= 4 {
				continue
			}
			path := win.ImagePath(p.PID)
			if path == "" {
				continue
			}
			for _, d := range dirs {
				if pathWithin(path, d) {
					a.Busy, a.Reason = true, "a game from the launcher is running ("+filepath.Base(path)+")"
					return a
				}
			}
		}
	}

	// Heavy disk/network traffic means a download, install or cloud-save
	// sync is in progress.
	before := map[uint32]uint64{}
	for _, pid := range transfer {
		if n, ok := win.IOBytes(pid); ok {
			before[pid] = n
		}
	}
	time.Sleep(3 * time.Second)
	var delta uint64
	for pid, was := range before {
		if n, ok := win.IOBytes(pid); ok && n > was {
			delta += n - was
		}
	}
	// With an install on record as unfinished, even a trickle counts: a
	// slow or throttled download must not be cut off.
	limit := uint64(6 << 20)
	if pendingInstall(manifestDir()) {
		limit = 128 << 10
	}
	if delta > limit {
		a.Busy, a.Reason = true, fmt.Sprintf("the launcher is transferring data (%d KB in 3 s): a download, install or save-sync seems to be in progress", delta>>10)
	}
	return a
}

// isLauncher reports whether a process is a launcher of this Windows
// session. With fast user switching another signed-in user's launcher may be
// running too: it cannot be looked into or closed from here, and it does
// not get in a hidden launcher's way.
func isLauncher(p win.ProcInfo) bool {
	return strings.EqualFold(p.Name, ExeName) && win.InThisSession(p.PID)
}

// visibleLauncherWindow reports whether any launcher process shows a window
// on the user's desktop.
func visibleLauncherWindow() bool {
	procs, err := win.Processes()
	if err != nil {
		return true
	}
	tree := map[uint32]bool{}
	for _, p := range procs {
		if isLauncher(p) {
			tree[p.PID] = true
		}
	}
	for _, p := range procs {
		if tree[p.PPID] && helperNames[strings.ToLower(p.Name)] {
			tree[p.PID] = true
		}
	}
	for _, w := range win.Windows() {
		if tree[w.PID] && w.Visible && (w.Iconic || (w.Right-w.Left >= 200 && w.Bottom-w.Top >= 150)) {
			return true
		}
	}
	return false
}

// KillLeftovers ends hidden launchers left behind by an earlier run of this
// program that ended abnormally. They are recognised by the DevTools switch
// only this program uses; one that shows a window is left alone. The caller
// must hold the session lock, so that another running copy's launcher is
// never taken for a leftover. It reports how many were ended.
func KillLeftovers() int {
	procs, err := win.Processes()
	if err != nil {
		return 0
	}
	var stale []uint32
	for _, p := range procs {
		if isLauncher(p) && strings.Contains(win.CommandLine(p.PID), "-cefdebug=") {
			stale = append(stale, p.PID)
		}
	}
	if len(stale) == 0 || visibleLauncherWindow() {
		return 0
	}
	killLaunchers(stale)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && launcherRunning() {
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(time.Second)
	return len(stale)
}

// killLaunchers ends the given launcher processes and their browser helpers.
func killLaunchers(pids []uint32) {
	procs, _ := win.Processes()
	tree := map[uint32]bool{}
	for _, pid := range pids {
		tree[pid] = true
	}
	var helpers []uint32
	for _, p := range procs {
		if tree[p.PPID] && helperNames[strings.ToLower(p.Name)] {
			helpers = append(helpers, p.PID)
		}
	}
	for _, pid := range pids {
		_ = win.Terminate(pid)
	}
	for _, pid := range helpers {
		_ = win.Terminate(pid)
	}
}

func launcherRunning() bool {
	procs, err := win.Processes()
	if err != nil {
		return true
	}
	for _, p := range procs {
		if isLauncher(p) {
			return true
		}
	}
	return false
}

// StopUserInstance ends the launcher the user left idle in the tray, so that
// a private hidden instance can take its place for a moment. The check that
// found it idle took a few seconds, so it is repeated here: if the user has
// opened the launcher in the meantime, nothing is touched. When the launcher
// is not gone afterwards, why says what stood in the way.
func StopUserInstance(a Activity) (stopped bool, why string) {
	if visibleLauncherWindow() {
		return false, "the launcher window is open"
	}
	// Only processes that still are the launcher (ids get reused).
	var pids []uint32
	if procs, err := win.Processes(); err == nil {
		was := map[uint32]bool{}
		for _, pid := range a.PIDs {
			was[pid] = true
		}
		for _, p := range procs {
			if was[p.PID] && isLauncher(p) {
				pids = append(pids, p.PID)
			}
		}
	}
	killLaunchers(pids)
	// Give Windows a moment to release the launcher's files and its
	// single-instance lock.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && launcherRunning() {
		time.Sleep(300 * time.Millisecond)
	}
	if launcherRunning() {
		// Typically a launcher running with administrator rights, which a
		// program without them may not end.
		return false, "the launcher is running and could not be closed (it may have been started with administrator rights)"
	}
	time.Sleep(1500 * time.Millisecond)
	return true, ""
}

// Running reports whether any launcher process exists.
func Running() bool { return launcherRunning() }

// StartForUser starts the launcher on the user's desktop, detached from this
// program. With silent it goes straight to the tray, as it does at login.
func StartForUser(inst Install, silent bool, extraArgs []string) error {
	args := append([]string{}, extraArgs...)
	if silent && inst.Gen == Legacy {
		args = append(args, "-silent")
	}
	return win.StartDetached(inst.Exe, args, inst.Dir)
}

// FreePort picks a free TCP port on localhost that the launcher's embedded
// browser accepts for DevTools (1024..65534).
func FreePort() (int, error) {
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if port >= 1024 && port <= 65534 {
			return port, nil
		}
	}
	return 0, errors.New("no usable local port found")
}

const desktopName = "EpicAutoClaim"

// Instance is a hidden launcher started by this program.
//
// It lives on a private Windows desktop that is never displayed: nothing it
// draws can reach the screen, the taskbar or the tray, and it cannot take
// the keyboard focus.
type Instance struct {
	Port int

	inst    Install
	flag    string // the DevTools switch, unique to this instance
	env     []string
	proc    *win.Process
	desktop *win.Desktop

	mu      sync.Mutex
	flagged map[uint32]bool
	// spawnMu is held while this program starts a process of its own and
	// while the process list is read, so that no reading ever shows such a
	// process before it has been placed among the instance's own.
	spawnMu sync.Mutex
}

// StartHidden starts the launcher invisibly with its embedded browser's
// DevTools endpoint on the given localhost port.
func StartHidden(inst Install, port int) (*Instance, error) {
	in := &Instance{Port: port, inst: inst, flagged: map[uint32]bool{}}
	args, env, flag := debugArgs(inst.Gen, port)
	in.flag, in.env = flag, env
	d, err := win.NewDesktop(desktopName)
	if err != nil {
		return nil, err
	}
	proc, err := win.Start(inst.Exe, args, inst.Dir, d.Name, env)
	if err != nil {
		d.Close()
		return nil, err
	}
	in.desktop, in.proc = d, proc
	return in, nil
}

// Summon asks the hidden launcher to bring up its main interface, exactly
// as a second start of the launcher does for a user whose launcher sits in
// the tray. The window appears on the private desktop only.
//
// With a com.epicgames.launcher:// link as argument it also asks the
// launcher to go to that place (a game's store page, for example), the way
// a link clicked in a web browser does.
func (in *Instance) Summon(link ...string) error {
	if in.proc == nil {
		return errors.New("hidden launcher is not running")
	}
	in.spawnMu.Lock()
	defer in.spawnMu.Unlock()
	_, err := in.proc.Spawn(in.inst.Exe, link, in.inst.Dir, in.desktop.Name, in.env)
	return err
}

// isMine reports whether a launcher process belongs to this instance: it is
// in the instance's job, or carries its unique DevTools switch (the launcher
// re-executes itself at start-up, keeping its arguments).
func (in *Instance) isMine(pid uint32, job map[uint32]bool) (mine, known bool) {
	if job[pid] {
		return true, true
	}
	in.mu.Lock()
	v, ok := in.flagged[pid]
	in.mu.Unlock()
	if ok {
		return v, true
	}
	cmd := win.CommandLine(pid)
	if cmd == "" {
		return false, false // gone already, or not readable: cannot tell
	}
	v = in.flag != "" && strings.Contains(cmd, in.flag)
	if !v && strings.Contains(cmd, "-Commandlet=") {
		// The launcher's own maintenance runs (self-update install) are
		// neither this instance nor the user asking for the launcher.
		return false, false
	}
	in.mu.Lock()
	in.flagged[pid] = v
	in.mu.Unlock()
	return v, true
}

// startedFromOutside reports whether a launcher process was started by
// something other than the launcher's own machinery (which re-executes and
// updates itself through helper processes): its parent is alive and is not
// an Epic program. When in doubt the answer is no: wrongly stepping aside
// would put a launcher window on the user's screen.
func startedFromOutside(p win.ProcInfo, byPID map[uint32]win.ProcInfo) bool {
	if p.PPID == uint32(os.Getpid()) {
		return false // what this program starts is never the user
	}
	parent, ok := byPID[p.PPID]
	if !ok {
		return false
	}
	n := strings.ToLower(parent.Name)
	return !strings.HasPrefix(n, "epic") && !strings.HasPrefix(n, "eos")
}

// launchers splits the running launcher processes into this instance's own
// and those started by someone else.
func (in *Instance) launchers() (mine, foreign []win.ProcInfo) {
	if in.proc == nil {
		return nil, nil
	}
	in.spawnMu.Lock()
	procs, err := win.Processes()
	job := in.proc.PIDs()
	in.spawnMu.Unlock()
	if err != nil {
		return nil, nil
	}
	byPID := make(map[uint32]win.ProcInfo, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	for _, p := range procs {
		if !isLauncher(p) {
			continue
		}
		m, known := in.isMine(p.PID, job)
		switch {
		case m:
			mine = append(mine, p)
		case known && startedFromOutside(p, byPID):
			foreign = append(foreign, p)
		}
	}
	return mine, foreign
}

// Alive reports whether the hidden launcher is running.
func (in *Instance) Alive() bool {
	mine, _ := in.launchers()
	return len(mine) > 0
}

// Foreign returns launcher processes started by someone else while the
// hidden instance runs: the user (or a game shortcut) asking for the
// launcher. Such a start is silently absorbed by the hidden instance, so the
// caller should step aside and start the launcher for the user.
func (in *Instance) Foreign() []win.ProcInfo {
	_, foreign := in.launchers()
	return foreign
}

// OnUserDesktop returns windows of the hidden launcher that are showing on
// the desktop the user sees. It must always be empty; it exists so that
// this can be verified, and corrected if it ever is not.
func (in *Instance) OnUserDesktop() []win.Window {
	if in.proc == nil {
		return nil
	}
	pids := in.proc.PIDs()
	mine, _ := in.launchers()
	for _, p := range mine {
		pids[p.PID] = true
	}
	var out []win.Window
	for _, w := range win.Windows() {
		if pids[w.PID] && w.Visible {
			out = append(out, w)
		}
	}
	return out
}

// HiddenWindows lists the launcher's windows on the private desktop.
func (in *Instance) HiddenWindows() []win.Window {
	if in.desktop == nil {
		return nil
	}
	return in.desktop.Windows()
}

// AskToClose asks the hidden launcher's windows to close, as a person
// closing them would. It reports whether there was anything to ask; the
// launcher then needs a few seconds to save its state and exit.
func (in *Instance) AskToClose() bool {
	if in == nil || in.proc == nil {
		return false
	}
	mine, _ := in.launchers()
	if len(mine) == 0 {
		return false
	}
	pids := in.proc.PIDs()
	for _, p := range mine {
		pids[p.PID] = true
	}
	n, err := in.desktop.RequestClose(pids)
	return err == nil && n > 0
}

// Stop shuts the hidden launcher down: first by asking its windows to close,
// then, after the grace period, by ending its processes.
func (in *Instance) Stop(grace time.Duration) {
	if in == nil || in.proc == nil {
		return
	}
	mine, _ := in.launchers()
	if grace > 0 && in.AskToClose() {
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) && in.Alive() {
			time.Sleep(250 * time.Millisecond)
		}
	}
	// Whatever is left, including a re-executed launcher that slipped out
	// of the job.
	if mine, _ = in.launchers(); len(mine) > 0 {
		pids := make([]uint32, 0, len(mine))
		for _, p := range mine {
			pids = append(pids, p.PID)
		}
		killLaunchers(pids)
	}
	in.proc.Kill()
	in.proc.WaitExit(5 * time.Second)
	in.proc.Close()
	in.proc = nil
	in.desktop.Close()
	in.desktop = nil
}

// WaitDevTools waits for the hidden launcher's DevTools endpoint. exited is
// true when the launcher went away instead (it closes itself when it wants
// to install an update of its own).
func (in *Instance) WaitDevTools(ctx context.Context, timeout time.Duration) (up, exited bool) {
	base := cdp.Base(in.Port)
	deadline := time.Now().Add(timeout)
	var goneSince time.Time
	for {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := cdp.Version(cctx, base)
		cancel()
		if err == nil {
			return true, false
		}
		if ctx.Err() != nil {
			return false, false
		}
		if in.Alive() {
			goneSince = time.Time{}
		} else {
			if goneSince.IsZero() {
				goneSince = time.Now()
			}
			// The launcher re-executes itself while starting; only a
			// lasting absence counts.
			if time.Since(goneSince) > 8*time.Second {
				return false, true
			}
		}
		if time.Now().After(deadline) {
			return false, false
		}
		select {
		case <-ctx.Done():
			return false, false
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// UpdateRun lets the launcher install its own pending update. That cannot
// happen on the private desktop (the updater does not start there), so for
// this one step the launcher runs on the normal desktop in its own
// start-in-tray mode, in which it shows no window; any window it or its
// updater shows regardless is hidden again at once.
func UpdateRun(ctx context.Context, inst Install, logf func(string, ...any)) error {
	if inst.Gen != Legacy {
		return errors.New("self-update handling is only known for the classic launcher")
	}
	if launcherRunning() {
		// Somebody else's launcher: a start here would only be passed on to
		// it, and would bring its window up.
		return ErrUserStarted
	}
	port, err := FreePort()
	if err != nil {
		return err
	}
	args, env, flag := debugArgs(inst.Gen, port)
	proc, err := win.Start(inst.Exe, args, inst.Dir, "", env)
	if err != nil {
		return err
	}
	defer proc.Close()

	// userCommand returns the command line of a launcher start that is not
	// part of this run: the user asking for the launcher.
	userCommand := func(procs []win.ProcInfo) (string, bool) {
		byPID := make(map[uint32]win.ProcInfo, len(procs))
		for _, p := range procs {
			byPID[p.PID] = p
		}
		for _, p := range procs {
			if !isLauncher(p) || !startedFromOutside(p, byPID) {
				continue
			}
			cmd := win.CommandLine(p.PID)
			if cmd != "" && !strings.Contains(cmd, flag) && !strings.Contains(cmd, "-Commandlet=") {
				return cmd, true
			}
		}
		return "", false
	}

	// The guard hides, within a few hundredths of a second, any window that
	// this run's own processes show, and it is also what notices the user
	// starting the launcher (a start that lives only for a moment before
	// the launcher running here absorbs it).
	stopGuard := make(chan struct{})
	userStart := make(chan string, 1)
	var guardDone sync.WaitGroup
	var hiddenCount int
	guardDone.Add(1)
	go func() {
		defer guardDone.Done()
		t := time.NewTicker(40 * time.Millisecond)
		defer t.Stop()
		extra := map[uint32]bool{}
		scanned := time.Time{}
		for {
			select {
			case <-stopGuard:
				return
			case <-t.C:
				// This run's own processes only: what it started (asked
				// afresh every time, so that a new process is known at
				// once), the launcher that comes back from the update
				// carrying this run's switch, and the updater in between.
				names := proc.PIDs()
				if time.Since(scanned) > 200*time.Millisecond {
					scanned = time.Now()
					if procs, err := win.Processes(); err == nil {
						extra = map[uint32]bool{}
						for _, p := range procs {
							switch {
							case strings.EqualFold(p.Name, "EpicGamesUpdater.exe") && win.InThisSession(p.PID):
								extra[p.PID] = true
							case isLauncher(p) && strings.Contains(win.CommandLine(p.PID), flag):
								extra[p.PID] = true
							}
						}
						if cmd, yes := userCommand(procs); yes {
							select {
							case userStart <- cmd:
							default:
							}
							return // their launcher from here on: nothing more is hidden
						}
					}
				}
				for pid := range extra {
					names[pid] = true
				}
				for _, w := range win.Windows() {
					if names[w.PID] && w.Visible {
						win.HideWindow(w.HWND)
						hiddenCount++
					}
				}
			}
		}
	}()
	guardStopped := false
	stopGuardNow := func() {
		if !guardStopped {
			guardStopped = true
			close(stopGuard)
			guardDone.Wait()
		}
	}
	defer stopGuardNow()

	// endRun ends every launcher that carries this run's switch, wherever
	// the updater started it from, and waits until none is left.
	endRun := func() {
		var mine []uint32
		if procs, err := win.Processes(); err == nil {
			for _, p := range procs {
				if isLauncher(p) && strings.Contains(win.CommandLine(p.PID), flag) {
					mine = append(mine, p.PID)
				}
			}
		}
		killLaunchers(mine)
		proc.Kill()
		stop := time.Now().Add(10 * time.Second)
		for time.Now().Before(stop) && launcherRunning() {
			time.Sleep(300 * time.Millisecond)
		}
	}

	// handOver is for when the user starts the launcher during this run.
	// Their start was absorbed by the launcher running here, which carries
	// this program's switches. It is ended, and the launcher is started
	// afresh the way they started it, so that what they get is entirely
	// their own.
	handOver := func(cmd string) error {
		stopGuardNow()
		endRun()
		_ = win.StartDetached(inst.Exe, userArgs(cmd), inst.Dir)
		logf("the launcher was started by the user while it was updating; leaving it to them")
		return ErrUserStarted
	}

	// The update is finished when the (restarted) launcher is back with its
	// DevTools endpoint up.
	// It has to stay up, too: a launcher that is about to restart for its
	// update may answer for a moment before it goes away again, and ending
	// it then would cut the update short.
	base := cdp.Base(port)
	started := time.Now()
	deadline := started.Add(10 * time.Minute)
	const steady = 10 * time.Second
	var upSince time.Time
	ok := false
	for time.Now().Before(deadline) && ctx.Err() == nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, verr := cdp.Version(cctx, base)
		cancel()
		switch {
		case verr != nil:
			upSince = time.Time{}
		case upSince.IsZero():
			upSince = time.Now()
		case time.Since(upSince) >= steady:
			ok = true
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
		case cmd := <-userStart:
			return handOver(cmd)
		case <-time.After(400 * time.Millisecond):
		}
	}
	select {
	case cmd := <-userStart:
		return handOver(cmd)
	default:
	}

	endRun()
	time.Sleep(1500 * time.Millisecond)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	stopGuardNow()
	if !ok {
		return errors.New("the launcher did not come back from its update within 10 minutes")
	}
	took := time.Since(started).Round(time.Second)
	if hiddenCount > 0 {
		logf("the launcher finished updating itself after %v (it tried to show a window %d time(s); each was hidden again within a few hundredths of a second)", took, hiddenCount)
	} else {
		logf("the launcher finished updating itself after %v (no window was shown)", took)
	}
	return nil
}

// UserArgs extracts, from the command line of a launcher start made by the
// user, the arguments worth repeating (in practice a com.epicgames.launcher://
// link from a game shortcut).
func UserArgs(commandLine string) []string {
	return userArgs(commandLine)
}
