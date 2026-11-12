//go:build windows

// Package win holds the Windows plumbing the claimer needs: a private
// desktop that is never shown, process control, and a few queries about what
// the user is doing.
package win

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procCreateDesktopW               = user32.NewProc("CreateDesktopW")
	procCloseDesktop                 = user32.NewProc("CloseDesktop")
	procEnumDesktopWindows           = user32.NewProc("EnumDesktopWindows")
	procSetThreadDesktop             = user32.NewProc("SetThreadDesktop")
	procGetWindowTextW               = user32.NewProc("GetWindowTextW")
	procIsIconic                     = user32.NewProc("IsIconic")
	procGetWindowRect                = user32.NewProc("GetWindowRect")
	procPostMessageW                 = user32.NewProc("PostMessageW")
	procShowWindow                   = user32.NewProc("ShowWindow")
	procGetLastInputInfo             = user32.NewProc("GetLastInputInfo")
	procGetTickCount                 = kernel32.NewProc("GetTickCount")
	procGetProcessIoCounters         = kernel32.NewProc("GetProcessIoCounters")
	procGetUserDefaultGeoName        = kernel32.NewProc("GetUserDefaultGeoName")
	procAttachConsole                = kernel32.NewProc("AttachConsole")
	procSHQueryUserNotificationState = shell32.NewProc("SHQueryUserNotificationState")
)

const (
	genericAll = 0x10000000
	wmClose    = 0x0010
	swHide     = 0
)

// ---------------------------------------------------------------- windows

// Window describes a top-level window.
type Window struct {
	HWND    uintptr
	PID     uint32
	Visible bool
	Iconic  bool
	Class   string
	Title   string
	Left    int32
	Top     int32
	Right   int32
	Bottom  int32
}

// Area is the window's size in pixels.
func (w Window) Area() int64 {
	return int64(w.Right-w.Left) * int64(w.Bottom-w.Top)
}

var (
	enumMu  sync.Mutex
	enumOut []Window
	enumCB  = windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		enumOut = append(enumOut, describe(hwnd))
		return 1
	})
)

func describe(hwnd uintptr) Window {
	w := Window{HWND: hwnd}
	_, _ = windows.GetWindowThreadProcessId(windows.HWND(hwnd), &w.PID)
	w.Visible = windows.IsWindowVisible(windows.HWND(hwnd))
	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		w.Iconic = true
	}
	buf := make([]uint16, 256)
	if n, _ := windows.GetClassName(windows.HWND(hwnd), &buf[0], int32(len(buf))); n > 0 {
		w.Class = windows.UTF16ToString(buf[:n])
	}
	if n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 {
		w.Title = windows.UTF16ToString(buf[:n])
	}
	var rc struct{ L, T, R, B int32 }
	if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r != 0 {
		w.Left, w.Top, w.Right, w.Bottom = rc.L, rc.T, rc.R, rc.B
	}
	return w
}

// Windows lists the top-level windows of the desktop the user sees.
func Windows() []Window {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumOut = nil
	_ = windows.EnumWindows(enumCB, nil)
	out := enumOut
	enumOut = nil
	return out
}

// HideWindow hides a window without closing it.
func HideWindow(hwnd uintptr) { _, _, _ = procShowWindow.Call(hwnd, swHide) }

// ---------------------------------------------------------------- desktop

// Desktop is a separate Windows desktop in the user's own session. Windows
// created on it are fully functional but are never drawn on the monitor,
// never appear in the taskbar and cannot take the keyboard focus, because
// only one desktop receives input and output at a time and this one is never
// switched to.
type Desktop struct {
	Name string
	h    uintptr
}

// NewDesktop creates the desktop, or opens it if it already exists.
func NewDesktop(name string) (*Desktop, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, callErr := procCreateDesktopW.Call(uintptr(unsafe.Pointer(n)), 0, 0, 0, genericAll, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateDesktop(%s): %w", name, callErr)
	}
	return &Desktop{Name: name, h: h}, nil
}

// Close releases the handle. Windows destroys the desktop once no handle and
// no process refers to it any more.
func (d *Desktop) Close() {
	if d != nil && d.h != 0 {
		_, _, _ = procCloseDesktop.Call(d.h)
		d.h = 0
	}
}

// Windows lists the top-level windows living on this desktop.
func (d *Desktop) Windows() []Window {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumOut = nil
	_, _, _ = procEnumDesktopWindows.Call(d.h, enumCB, 0)
	out := enumOut
	enumOut = nil
	return out
}

// RequestClose asks the given processes' windows on this desktop to close,
// as if the user had clicked their close buttons. Window messages cannot
// cross desktops, so this runs on a thread that is attached to the desktop
// for the duration of the call.
func (d *Desktop) RequestClose(pids map[uint32]bool) (posted int, err error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	h := d.h
	go func() {
		// Deliberately never unlocked: the OS thread ends with this
		// goroutine, and its desktop assignment with it.
		runtime.LockOSThread()
		if r, _, callErr := procSetThreadDesktop.Call(h); r == 0 {
			done <- result{err: fmt.Errorf("SetThreadDesktop: %w", callErr)}
			return
		}
		enumMu.Lock()
		enumOut = nil
		_, _, _ = procEnumDesktopWindows.Call(h, enumCB, 0)
		wins := enumOut
		enumOut = nil
		enumMu.Unlock()
		n := 0
		for _, w := range wins {
			if pids[w.PID] {
				if r, _, _ := procPostMessageW.Call(w.HWND, wmClose, 0, 0); r != 0 {
					n++
				}
			}
		}
		done <- result{n: n}
	}()
	r := <-done
	return r.n, r.err
}

// ---------------------------------------------------------------- processes

// Process is a child process started by Start, together with everything it
// spawns: the whole tree lives in a job object that Windows tears down when
// this program exits, however it exits. A hidden launcher can therefore
// never be left behind.
type Process struct {
	PID uint32
	h   windows.Handle
	job windows.Handle
}

// envBlock builds a Windows environment block: this process's environment
// with the given "NAME=value" entries added (replacing same-named ones).
// It returns nil when there is nothing to add, meaning "inherit as is".
func envBlock(extra []string) *uint16 {
	if len(extra) == 0 {
		return nil
	}
	override := map[string]bool{}
	for _, kv := range extra {
		if name, _, ok := strings.Cut(kv, "="); ok {
			override[strings.ToUpper(name)] = true
		}
	}
	var all []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "" && override[strings.ToUpper(name)] {
			continue
		}
		all = append(all, kv)
	}
	all = append(all, extra...)
	var block []uint16
	for _, kv := range all {
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	block = append(block, 0)
	return &block[0]
}

// create starts a process suspended, puts it in job, and lets it run.
func create(exe string, args []string, dir, desktop string, env []string, job windows.Handle) (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return pi, err
	}
	cmdPtr, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return pi, err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return pi, err
		}
	}
	si := windows.StartupInfo{}
	si.Cb = uint32(unsafe.Sizeof(si))
	if desktop != "" {
		if si.Desktop, err = windows.UTF16PtrFromString(desktop); err != nil {
			return pi, err
		}
	}
	// Ordinary priority, stated outright: a program started by a scheduled
	// task may itself be running below normal, and what it starts would
	// otherwise inherit that.
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NEW_PROCESS_GROUP | windows.NORMAL_PRIORITY_CLASS)
	if err := windows.CreateProcess(exePtr, cmdPtr, nil, nil, false, flags, envBlock(env), dirPtr, &si, &pi); err != nil {
		if errors.Is(err, windows.ERROR_ELEVATION_REQUIRED) {
			return pi, fmt.Errorf("start %s: it requires administrator rights, which this program does not have", filepath.Base(exe))
		}
		return pi, fmt.Errorf("start %s: %w", filepath.Base(exe), err)
	}
	// Suspended until it is in the job, so no child can escape it.
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
		return pi, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	_, _ = windows.ResumeThread(pi.Thread)
	_ = windows.CloseHandle(pi.Thread)
	return pi, nil
}

// Start runs exe with args. If desktop is non-empty the process (and its
// children) are placed on that desktop. env holds extra "NAME=value"
// environment entries.
func Start(exe string, args []string, dir, desktop string, env []string) (*Process, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	if err := setJobLimits(job, windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	pi, err := create(exe, args, dir, desktop, env, job)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &Process{PID: pi.ProcessId, h: pi.Process, job: job}, nil
}

// Spawn starts another process inside the same job (so it is cleaned up
// together with the first) and returns its id.
func (p *Process) Spawn(exe string, args []string, dir, desktop string, env []string) (uint32, error) {
	if p == nil || p.job == 0 {
		return 0, errors.New("process tree is closed")
	}
	pi, err := create(exe, args, dir, desktop, env, p.job)
	if err != nil {
		return 0, err
	}
	_ = windows.CloseHandle(pi.Process)
	return pi.ProcessId, nil
}

// jobIDList is JOBOBJECT_BASIC_PROCESS_ID_LIST: two DWORDs followed by
// ULONG_PTR ids.
type jobIDList struct {
	Assigned uint32
	InList   uint32
	IDs      [1024]uintptr
}

// The Windows calls below take the address of their buffer as a plain
// number, which hides it from Go's runtime. A buffer on a goroutine stack
// could be moved while such a call is being set up, so these buffers live
// in package-level variables (never on a stack), one call at a time.
var (
	jobCallMu   sync.Mutex
	jobIDBuf    jobIDList
	jobLimitBuf windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
)

// setJobLimits applies limit flags to a job.
func setJobLimits(job windows.Handle, flags uint32) error {
	jobCallMu.Lock()
	defer jobCallMu.Unlock()
	jobLimitBuf = windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	jobLimitBuf.BasicLimitInformation.LimitFlags = flags
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&jobLimitBuf)), uint32(unsafe.Sizeof(jobLimitBuf)))
	return err
}

// PIDs returns the ids of every live process in the tree.
func (p *Process) PIDs() map[uint32]bool {
	out := map[uint32]bool{}
	if p == nil || p.job == 0 {
		return out
	}
	jobCallMu.Lock()
	defer jobCallMu.Unlock()
	jobIDBuf.Assigned, jobIDBuf.InList = 0, 0
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&jobIDBuf)), uint32(unsafe.Sizeof(jobIDBuf)), nil); err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return out
	}
	n := jobIDBuf.InList
	if n > uint32(len(jobIDBuf.IDs)) {
		n = uint32(len(jobIDBuf.IDs))
	}
	for i := uint32(0); i < n; i++ {
		out[uint32(jobIDBuf.IDs[i])] = true
	}
	return out
}

// Alive reports whether any process of the tree is still running.
func (p *Process) Alive() bool { return len(p.PIDs()) > 0 }

// WaitExit waits until the whole tree has exited or the timeout passes.
func (p *Process) WaitExit(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !p.Alive() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Kill terminates the whole tree.
func (p *Process) Kill() {
	if p != nil && p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
}

// Close kills whatever is left and releases the handles.
func (p *Process) Close() {
	if p == nil {
		return
	}
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
	if p.h != 0 {
		_ = windows.CloseHandle(p.h)
		p.h = 0
	}
}

// ProcInfo is one entry of the system process list.
type ProcInfo struct {
	PID  uint32
	PPID uint32
	Name string // image file name, e.g. "EpicGamesLauncher.exe"
	Path string // full image path; empty when access is denied
}

// Processes lists the running processes.
func Processes() ([]ProcInfo, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []ProcInfo
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out = append(out, ProcInfo{PID: e.ProcessID, PPID: e.ParentProcessID, Name: windows.UTF16ToString(e.ExeFile[:])})
	}
	return out, nil
}

var ownSession = sync.OnceValue(func() uint32 {
	var sid uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid)
	return sid
})

// InThisSession reports whether a process belongs to the Windows session
// (the signed-in user's desktop) this program runs in. A process that may
// not even be asked is another user's, so the answer is no; when the
// question fails for any other reason the process is taken to be local.
func InThisSession(pid uint32) bool {
	var sid uint32
	if err := windows.ProcessIdToSessionId(pid, &sid); err != nil {
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true
		}
		// A process of another account, which may not be opened. It can
		// still be in this session ("Run as different user"); the system's
		// process table says where it is without opening it.
		if s, ok := sessionFromProcessTable(pid); ok {
			return s == ownSession()
		}
		return false
	}
	return sid == ownSession()
}

// sessionFromProcessTable looks a process's session up in the system-wide
// process table, which needs no access to the process itself.
func sessionFromProcessTable(pid uint32) (uint32, bool) {
	size := uint32(1 << 18)
	for try := 0; try < 6; try++ {
		buf := make([]byte, size)
		var need uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &need)
		if err == windows.STATUS_INFO_LENGTH_MISMATCH || err == windows.STATUS_BUFFER_TOO_SMALL {
			if need > size {
				size = need
			}
			size += 1 << 16
			continue
		}
		if err != nil {
			return 0, false
		}
		entry := unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{})
		for off := uintptr(0); off+entry <= uintptr(len(buf)); {
			p := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
			if uint32(p.UniqueProcessID) == pid {
				return p.SessionID, true
			}
			if p.NextEntryOffset == 0 {
				break
			}
			off += uintptr(p.NextEntryOffset)
		}
		return 0, false
	}
	return 0, false
}

// ImagePath returns the full path of a process's executable.
func ImagePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// IOBytes returns the total bytes a process has read and written so far.
func IOBytes(pid uint32) (uint64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var c windows.IO_COUNTERS
	if r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&c))); r == 0 {
		return 0, false
	}
	return c.ReadTransferCount + c.WriteTransferCount, true
}

// Terminate force-ends a process by id.
func Terminate(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

// StartDetached launches a program on the user's normal desktop, outside any
// job, so that it outlives this program.
func StartDetached(exe string, args []string, dir string) error {
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	cmdPtr, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return err
		}
	}
	si := windows.StartupInfo{}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB | windows.NORMAL_PRIORITY_CLASS)
	err = windows.CreateProcess(exePtr, cmdPtr, nil, nil, false, flags, nil, dirPtr, &si, &pi)
	if err != nil {
		// Breaking away is refused when this program itself runs inside a
		// job that forbids it; a plain start is the next best thing.
		flags &^= windows.CREATE_BREAKAWAY_FROM_JOB
		err = windows.CreateProcess(exePtr, cmdPtr, nil, nil, false, flags, nil, dirPtr, &si, &pi)
	}
	if err != nil {
		return fmt.Errorf("start %s: %w", filepath.Base(exe), err)
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process)
	return nil
}

// ---------------------------------------------------------------- user state

// IdleFor reports how long ago the user last touched keyboard or mouse.
func IdleFor() time.Duration {
	var lii struct {
		Size uint32
		Time uint32
	}
	lii.Size = uint32(unsafe.Sizeof(lii))
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-lii.Time) * time.Millisecond
}

// FullscreenActivity reports whether the user is in a full-screen game or
// presentation, according to the shell.
func FullscreenActivity() bool {
	var state uint32
	if hr, _, _ := procSHQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&state))); hr != 0 {
		return false
	}
	// 2 QUNS_BUSY, 3 QUNS_RUNNING_D3D_FULL_SCREEN, 4 QUNS_PRESENTATION_MODE
	return state == 2 || state == 3 || state == 4
}

// Country returns the user's region as a two-letter ISO code, or "".
func Country() string {
	if procGetUserDefaultGeoName.Find() != nil {
		return ""
	}
	buf := make([]uint16, 16)
	n, _, _ := procGetUserDefaultGeoName.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	s := strings.ToUpper(windows.UTF16ToString(buf))
	if len(s) != 2 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return "" // numeric UN M.49 region or unknown ("XX")
	}
	if s == "XX" {
		return ""
	}
	return s
}

// ---------------------------------------------------------------- misc

// SingleInstance takes a named lock. ok is false when another process
// already holds it. Keep the returned handle open for the process lifetime.
func SingleInstance(name string) (release func(), ok bool, err error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}
	h, err := windows.CreateMutex(nil, false, n)
	if h == 0 {
		return nil, false, fmt.Errorf("CreateMutex: %w", err)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(h)
		return nil, false, nil
	}
	return func() { _ = windows.CloseHandle(h) }, true, nil
}

var (
	modwtsapi32                     = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = modwtsapi32.NewProc("WTSQuerySessionInformationW")
)

// wtsInfo is WTSINFOW from wtsapi32.h (what WTSQuerySessionInformation
// returns for the WTSSessionInfo class).
type wtsInfo struct {
	State                   uint32
	SessionID               uint32
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
	WinStationName          [32]uint16 // WINSTATIONNAME_LENGTH
	Domain                  [17]uint16 // DOMAIN_LENGTH
	UserName                [21]uint16 // USERNAME_LENGTH + 1
	ConnectTime             int64      // these five are FILETIME values (UTC)
	DisconnectTime          int64
	LastInputTime           int64
	LogonTime               int64
	CurrentTime             int64
}

// SignedInSince returns when the user signed in to the Windows session this
// program runs in. ok is false when Windows will not say.
func SignedInSince() (t time.Time, ok bool) {
	const (
		wtsCurrentSession = ^uint32(0) // WTS_CURRENT_SESSION
		wtsSessionInfo    = 24         // WTS_INFO_CLASS: WTSSessionInfo
	)
	var buf *wtsInfo
	var n uint32
	r, _, _ := procWTSQuerySessionInformationW.Call(0, uintptr(wtsCurrentSession), wtsSessionInfo,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return time.Time{}, false
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	if uintptr(n) < unsafe.Sizeof(wtsInfo{}) || buf.LogonTime <= 0 {
		return time.Time{}, false
	}
	ft := windows.Filetime{LowDateTime: uint32(buf.LogonTime), HighDateTime: uint32(buf.LogonTime >> 32)}
	t = time.Unix(0, ft.Nanoseconds())
	// A value that is plainly not a sign-in time of this session.
	if t.After(time.Now().Add(time.Minute)) || t.Before(time.Now().Add(-400*24*time.Hour)) {
		return time.Time{}, false
	}
	return t, true
}

var procSetProcessInformation = kernel32.NewProc("SetProcessInformation")

// processPowerThrottling is PROCESS_POWER_THROTTLING_STATE.
type processPowerThrottling struct {
	Version     uint32
	ControlMask uint32
	StateMask   uint32
}

// Background puts this process into the background while it has nothing to
// do, and takes it out again: lowest priority for the processor, the disk
// and memory, Windows' "efficiency mode" where there is one, and its
// memory handed back to Windows (a waiting process then holds on to very
// little). It must be taken out of the background before it starts another
// program, which would otherwise inherit the low disk and memory priority.
func Background(on bool) {
	const (
		processModeBackgroundEnd = 0x00200000
		powerThrottlingClass     = 4 // ProcessPowerThrottling
		throttleExecutionSpeed   = 0x1
	)
	h := windows.CurrentProcess()
	eco := processPowerThrottling{Version: 1, ControlMask: throttleExecutionSpeed}
	if on {
		eco.StateMask = throttleExecutionSpeed
		_ = windows.SetPriorityClass(h, windows.PROCESS_MODE_BACKGROUND_BEGIN)
	} else {
		_ = windows.SetPriorityClass(h, processModeBackgroundEnd)
	}
	if procSetProcessInformation.Find() == nil {
		_, _, _ = procSetProcessInformation.Call(uintptr(h), powerThrottlingClass, uintptr(unsafe.Pointer(&eco)), unsafe.Sizeof(eco))
	}
	if on {
		// Hand back every page that is not needed right now.
		_ = windows.SetProcessWorkingSetSizeEx(h, ^uintptr(0), ^uintptr(0), 0)
	}
}

// LockHeld reports whether some process holds the named single-instance
// lock, without creating it. A lock that exists but may not be opened (its
// holder runs with higher rights) counts as held.
func LockHeld(name string) bool {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, n)
	if err == nil {
		_ = windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// AttachParentConsole connects a GUI-subsystem process to the console of the
// program that started it, so command-line use can print. It reports whether
// a console was attached.
func AttachParentConsole() bool {
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	return r != 0
}

// MessageBox shows a simple informational dialog.
func MessageBox(title, text string, isError bool) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	flags := uint32(windows.MB_OK | windows.MB_ICONINFORMATION | windows.MB_SETFOREGROUND)
	if isError {
		flags = windows.MB_OK | windows.MB_ICONERROR | windows.MB_SETFOREGROUND
	}
	_, _ = windows.MessageBox(0, m, t, flags)
}

// LocalAppData returns %LOCALAPPDATA%.
func LocalAppData() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
}

// CommandLine returns the command line a process was started with, or "".
func CommandLine(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	size := uint32(8 << 10)
	for attempt := 0; attempt < 3; attempt++ {
		// The kernel writes a UNICODE_STRING header followed by the text,
		// with the header's pointer aimed into this same buffer.
		buf := make([]uint64, (size+7)/8)
		var need uint32
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)*8), &need)
		if err == nil {
			us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
			s := us.String()
			runtime.KeepAlive(buf)
			return s
		}
		if need <= size {
			return ""
		}
		size = need
	}
	return ""
}

// StopSignal is a named, system-wide "please exit" flag shared by the
// background process and the commands that control it.
type StopSignal struct{ h windows.Handle }

// NewStopSignal creates (or opens) the flag in the cleared state.
func NewStopSignal(name string) (*StopSignal, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateEvent(nil, 1, 0, n)
	if h == 0 {
		return nil, fmt.Errorf("CreateEvent: %w", err)
	}
	_ = windows.ResetEvent(h)
	return &StopSignal{h: h}, nil
}

// Wait blocks until the flag is raised.
func (s *StopSignal) Wait() {
	_, _ = windows.WaitForSingleObject(s.h, windows.INFINITE)
}

// Close releases the handle.
func (s *StopSignal) Close() { _ = windows.CloseHandle(s.h) }

// WaitExitOrStop blocks until the process with the given id has ended or the
// stop flag is raised, whichever comes first. It reports whether it was the
// stop flag. A process that cannot be opened is taken to have ended already.
func WaitExitOrStop(pid uint32, stop *StopSignal) (stopped bool) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil || h == 0 {
		return false
	}
	defer windows.CloseHandle(h)
	handles := []windows.Handle{h}
	if stop != nil {
		handles = append(handles, stop.h)
	}
	ev, err := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
	return err == nil && ev == windows.WAIT_OBJECT_0+1
}

// RaiseStopSignal raises the flag if a process is listening for it. It
// reports whether there was one.
func RaiseStopSignal(name string) bool {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, n)
	if err != nil || h == 0 {
		return false
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h) == nil
}

// ConsoleOut returns a writer for command-line output. A program built for
// the Windows GUI subsystem has no console of its own: output goes to the
// handle it was given (a pipe or file, when redirected), else to the console
// of the program that started it, else nowhere.
func ConsoleOut() *os.File {
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && h != 0 && h != windows.InvalidHandle {
		if t, err := windows.GetFileType(h); err == nil && t != windows.FILE_TYPE_UNKNOWN {
			return os.Stdout
		}
	}
	if !AttachParentConsole() {
		return nil
	}
	name, _ := windows.UTF16PtrFromString("CONOUT$")
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(h), "CONOUT$")
}
