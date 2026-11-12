//go:build windows

package launcher

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

// Install describes where the launcher lives on this PC.
type Install struct {
	Exe string // full path of EpicGamesLauncher.exe
	Dir string // its folder, used as the working directory
}

// ErrNotInstalled is returned when the launcher cannot be found.
var ErrNotInstalled = errors.New("the Epic Games Launcher is not installed (EpicGamesLauncher.exe was not found)")

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

// uninstallRoots returns the install roots recorded by the launcher's
// installer under the "Uninstall" registry keys.
func uninstallRoots() []string {
	var out []string
	const base = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
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
// configuration file and must point at EpicGamesLauncher.exe.
func Locate(override string) (Install, error) {
	if override = strings.TrimSpace(override); override != "" {
		if !isLauncherExe(override) || !fileExists(override) {
			return Install{}, fmt.Errorf("launcherPath %q is not an existing %s", override, ExeName)
		}
		return Install{Exe: override, Dir: filepath.Dir(override)}, nil
	}

	var candidates []string
	add := func(p string) {
		if p != "" {
			candidates = append(candidates, p)
		}
	}

	// 1. A launcher that is running right now is the most certain answer.
	if procs, err := win.Processes(); err == nil {
		for _, p := range procs {
			if strings.EqualFold(p.Name, ExeName) {
				add(win.ImagePath(p.PID))
			}
		}
	}
	// 2. The com.epicgames.launcher:// link handler the installer registers.
	add(exeFromCommand(regString(registry.CLASSES_ROOT, `com.epicgames.launcher\shell\open\command`, "", 0)))
	// 3. The "run when my computer starts" entry.
	add(exeFromCommand(regString(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, "EpicGamesLauncher", 0)))
	// 4. The installer's uninstall record.
	for _, root := range uninstallRoots() {
		candidates = append(candidates, exeUnder(root)...)
	}
	// 5. The default locations.
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "ProgramW6432"} {
		if base := os.Getenv(env); base != "" {
			candidates = append(candidates, exeUnder(base+`\Epic Games`)...)
		}
	}

	for _, c := range candidates {
		if isLauncherExe(c) && fileExists(c) {
			return Install{Exe: c, Dir: filepath.Dir(c)}, nil
		}
	}
	return Install{}, ErrNotInstalled
}

// manifestDir is where the launcher records the games it has installed.
func manifestDir() string {
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
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
	"epicwebhelper.exe":       true,
	"unrealcefsubprocess.exe": true,
}

// Inspect reports whether the launcher is running and whether the user (or
// a download) is using it. exclude lists processes to ignore: the hidden
// instance this program itself started.
func Inspect(exclude map[uint32]bool) Activity {
	var a Activity
	procs, err := win.Processes()
	if err != nil {
		// Unknown is treated as busy: never disturb what cannot be seen.
		return Activity{Running: true, Busy: true, Reason: "could not list processes: " + err.Error()}
	}
	tree := map[uint32]bool{}
	for _, p := range procs {
		if !exclude[p.PID] && strings.EqualFold(p.Name, ExeName) {
			a.PIDs = append(a.PIDs, p.PID)
			tree[p.PID] = true
		}
	}
	if len(a.PIDs) == 0 {
		return a
	}
	a.Running = true
	for _, p := range procs {
		if !exclude[p.PID] && tree[p.PPID] && helperNames[strings.ToLower(p.Name)] {
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

	// Heavy disk/network traffic means a download or install is in progress.
	before := map[uint32]uint64{}
	for pid := range tree {
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
	if delta > 6<<20 {
		a.Busy, a.Reason = true, fmt.Sprintf("the launcher is transferring data (%d MB in 3s): a download or install seems to be in progress", delta>>20)
	}
	return a
}

// StopUserInstance ends the launcher the user left running in the tray, so
// that a private hidden instance can take its place for a moment.
func StopUserInstance(a Activity) {
	procs, _ := win.Processes()
	tree := map[uint32]bool{}
	for _, pid := range a.PIDs {
		tree[pid] = true
	}
	var helpers []uint32
	for _, p := range procs {
		if tree[p.PPID] && helperNames[strings.ToLower(p.Name)] {
			helpers = append(helpers, p.PID)
		}
	}
	for _, pid := range a.PIDs {
		_ = win.Terminate(pid)
	}
	for _, pid := range helpers {
		_ = win.Terminate(pid)
	}
	// Give Windows a moment to release the launcher's files and its
	// single-instance lock.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !Inspect(nil).Running {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
}

// StartForUser starts the launcher on the user's desktop, detached from this
// program. With silent it goes straight to the tray, as it does at login.
func StartForUser(inst Install, silent bool, extraArgs []string) error {
	args := append([]string{}, extraArgs...)
	if silent {
		args = append(args, "-silent")
	}
	return win.StartDetached(inst.Exe, args, inst.Dir)
}

// FreePort picks a free TCP port on localhost.
func FreePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Hide modes.
const (
	// ModeDesktop runs the launcher on a private desktop that is never
	// displayed. Nothing of it can appear on screen.
	ModeDesktop = "desktop"
	// ModeTray starts the launcher minimised to the tray ("-silent") on the
	// normal desktop and hides any window it tries to show.
	ModeTray = "tray"
)

const desktopName = "EpicAutoClaim"

// Instance is a hidden launcher started by this program.
type Instance struct {
	Port int
	Mode string

	inst    Install
	proc    *win.Process
	desktop *win.Desktop

	guardStop chan struct{}
	guardDone sync.WaitGroup
}

// StartHidden starts the launcher invisibly with its embedded browser's
// DevTools endpoint on the given localhost port.
func StartHidden(inst Install, port int, mode string) (*Instance, error) {
	in := &Instance{Port: port, Mode: mode, inst: inst}
	args := []string{fmt.Sprintf("-cefdebug=%d", port)}
	switch mode {
	case ModeTray:
		args = append(args, "-silent")
		proc, err := win.Start(inst.Exe, args, inst.Dir, "")
		if err != nil {
			return nil, err
		}
		in.proc = proc
		in.guardStop = make(chan struct{})
		in.guardDone.Add(1)
		go in.guard()
	default:
		in.Mode = ModeDesktop
		d, err := win.NewDesktop(desktopName)
		if err != nil {
			return nil, err
		}
		proc, err := win.Start(inst.Exe, args, inst.Dir, d.Name)
		if err != nil {
			d.Close()
			return nil, err
		}
		in.desktop, in.proc = d, proc
	}
	return in, nil
}

// guard keeps the tray-mode launcher off the screen: any window it shows is
// hidden again at once.
func (in *Instance) guard() {
	defer in.guardDone.Done()
	t := time.NewTicker(60 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-in.guardStop:
			return
		case <-t.C:
			pids := in.proc.PIDs()
			if len(pids) == 0 {
				continue
			}
			for _, w := range win.Windows() {
				if pids[w.PID] && w.Visible {
					win.HideWindow(w.HWND)
				}
			}
		}
	}
}

// PIDs are the processes of the hidden launcher.
func (in *Instance) PIDs() map[uint32]bool { return in.proc.PIDs() }

// Alive reports whether the hidden launcher is still running.
func (in *Instance) Alive() bool { return in.proc.Alive() }

// VisibleWindows returns launcher windows that are showing on the user's
// desktop. It must always be empty; it exists so that this can be verified.
func (in *Instance) VisibleWindows() []win.Window {
	pids := in.proc.PIDs()
	var out []win.Window
	for _, w := range win.Windows() {
		if pids[w.PID] && w.Visible {
			out = append(out, w)
		}
	}
	return out
}

// Stop shuts the hidden launcher down: first by asking its windows to close,
// then, after the grace period, by ending its processes.
func (in *Instance) Stop(grace time.Duration) {
	if in == nil || in.proc == nil {
		return
	}
	if in.guardStop != nil {
		close(in.guardStop)
		in.guardDone.Wait()
		in.guardStop = nil
	}
	if in.desktop != nil && grace > 0 && in.proc.Alive() {
		if n, err := in.desktop.RequestClose(in.proc.PIDs()); err == nil && n > 0 {
			in.proc.WaitExit(grace)
		}
	}
	in.proc.Kill()
	in.proc.WaitExit(5 * time.Second)
	in.proc.Close()
	in.proc = nil
	if in.desktop != nil {
		in.desktop.Close()
		in.desktop = nil
	}
}

// Foreign returns launcher processes that were started by someone else while
// the hidden instance runs: the user (or a game shortcut) asking for the
// launcher.
func (in *Instance) Foreign() []win.ProcInfo {
	procs, err := win.Processes()
	if err != nil {
		return nil
	}
	mine := in.proc.PIDs()
	var out []win.ProcInfo
	for _, p := range procs {
		if strings.EqualFold(p.Name, ExeName) && !mine[p.PID] {
			out = append(out, p)
		}
	}
	return out
}
