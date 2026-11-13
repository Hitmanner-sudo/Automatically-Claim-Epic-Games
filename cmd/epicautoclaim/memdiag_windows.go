//go:build windows && memdiag

package main

// The "memdiag" build (go build -tags memdiag) is for measuring where the
// background process's memory goes; it is never released. It lets the
// tuning be set from the environment and writes memory figures into the
// log at marked moments. See .github/workflows/mem.yml.

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/win"
)

func envInt(name string, into *int) {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		*into = v
	}
}

func memKnobs(t *tuning) {
	envInt("EAC_PROCS", &t.procs) // 0: leave it to the runtime (or GOMAXPROCS)
	envInt("EAC_GC", &t.gcPercent)
	envInt("EAC_CLAIM_GC", &t.claimGCPercent)
	stages := 0
	if t.stages {
		stages = 1
	}
	envInt("EAC_STAGES", &stages)
	t.stages = stages != 0
	envInt("EAC_REST_GC", &memRestGC)
	mb := int(t.limit >> 20)
	envInt("EAC_LIMIT_MB", &mb)
	t.limit = int64(mb) << 20
}

// processMemoryCountersEx2 is PROCESS_MEMORY_COUNTERS_EX2.
type processMemoryCountersEx2 struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
	privateWorkingSetSize      uintptr
	sharedCommitUsage          uint64
}

// memRestGC: 1 leaves the garbage collector on while the program waits, as
// before, to measure what switching it off saves.
var memRestGC int

// memStart is when the program started, near enough.
var memStart = time.Now()

var (
	procK32GetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")
	memMu                       sync.Mutex
)

func mb(n uintptr) string { return fmt.Sprintf("%.1f", float64(n)/(1<<20)) }

// memMark writes one line of memory figures into the log.
func memMark(logf func(string, ...any), label string) {
	memMu.Lock()
	defer memMu.Unlock()
	if memLog == nil {
		memLog = logf // (the first mark is made before anything is downloaded)
	}
	var c processMemoryCountersEx2
	private := "?"
	c.cb = uint32(unsafe.Sizeof(c))
	r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r != 0 {
		private = mb(c.privateWorkingSetSize)
	} else {
		// An older Windows: without the last two fields.
		c.cb = uint32(unsafe.Offsetof(c.privateWorkingSetSize))
		procK32GetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// (No runtime/pprof here: with it in the program the runtime keeps a
	// table for memory profiling, 1.4 MB that a released build does not have.)
	logf("mem %-22s t=%dms faults=%d cap=%s ws=%s privateWS=%s peakWS=%s commit=%s | go: mapped=%.1f heapInuse=%.1f heapIdle-released=%.1f stacks=%.1f gcSys=%.1f spans+caches=%.1f profiling=%.1f other=%.1f totalAlloc=%.1f numGC=%d | goroutines=%d procs=%d modules=%d",
		label, time.Since(memStart).Milliseconds(), c.pageFaultCount, mb(win.WorkingSetCap()), mb(c.workingSetSize), private, mb(c.peakWorkingSetSize), mb(c.privateUsage),
		float64(m.Sys-m.HeapReleased)/(1<<20), float64(m.HeapInuse)/(1<<20), float64(m.HeapIdle-m.HeapReleased)/(1<<20),
		float64(m.StackSys)/(1<<20), float64(m.GCSys)/(1<<20), float64(m.MSpanSys+m.MCacheSys)/(1<<20), float64(m.BuckHashSys)/(1<<20), float64(m.OtherSys)/(1<<20),
		float64(m.TotalAlloc)/(1<<20), m.NumGC,
		runtime.NumGoroutine(), runtime.GOMAXPROCS(0), len(modules()))
	if strings.HasPrefix(label, "end") {
		logf("mem modules: %s", strings.Join(modules(), " "))
	}
}

// modules lists the DLLs loaded into this process.
func modules() []string {
	h := windows.CurrentProcess()
	mods := make([]windows.Handle, 512)
	var needed uint32
	if windows.EnumProcessModules(h, &mods[0], uint32(len(mods))*uint32(unsafe.Sizeof(mods[0])), &needed) != nil {
		return nil
	}
	n := int(needed) / int(unsafe.Sizeof(mods[0]))
	if n > len(mods) {
		n = len(mods)
	}
	var names []string
	for _, mod := range mods[:n] {
		buf := make([]uint16, 260)
		if windows.GetModuleBaseName(h, mod, &buf[0], uint32(len(buf))) == nil {
			names = append(names, strings.ToLower(windows.UTF16ToString(buf)))
		}
	}
	sort.Strings(names)
	return names
}

// memLog is where the marks of a download go (set by the first mark).
var memLog func(string, ...any)

type markingTransport struct{ base http.RoundTripper }

func (t markingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mark := func(label string) {
		memMu.Lock()
		logf := memLog
		memMu.Unlock()
		if logf != nil {
			memMark(logf, label)
		}
	}
	mark("http: before")
	trace := &httptrace.ClientTrace{
		DNSDone:              func(httptrace.DNSDoneInfo) { mark("http: name looked up") },
		ConnectDone:          func(string, string, error) { mark("http: connected") },
		TLSHandshakeStart:    func() { mark("http: TLS starts") },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { mark("http: TLS done") },
		WroteRequest:         func(httptrace.WroteRequestInfo) { mark("http: request sent") },
		GotFirstResponseByte: func() { mark("http: first byte") },
	}
	res, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	proto := "?"
	if res != nil {
		proto = res.Proto
	}
	mark("http: headers " + proto)
	return res, err
}

// memClient reads the list as feed.Client's own does, through a transport
// that marks the stages. (The marks are made before the working set is
// handed back at the same stage: a trace attached later runs first.)
func memClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: markingTransport{base: http.DefaultTransport}}
}

// memRestingAllowed: see memRestGC.
func memRestingAllowed() bool { return memRestGC == 0 }
