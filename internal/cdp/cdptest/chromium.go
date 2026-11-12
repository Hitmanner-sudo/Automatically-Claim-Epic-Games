// Package cdptest starts a real headless Chromium for integration tests.
package cdptest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/cdp"
)

func findChromium() string {
	if p := os.Getenv("CHROMIUM_PATH"); p != "" {
		return p
	}
	candidates := []string{
		"/opt/pw-browsers/chromium",
		"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome",
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, c := range candidates {
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// Start launches headless Chromium with a DevTools port and returns the HTTP
// base URL of its endpoint. The test is skipped when no browser is available.
func Start(t testing.TB) string {
	t.Helper()
	// CDP_BASE points the tests at a browser that is already running: in CI,
	// the Epic Games Launcher's own (Chromium 90), so that what the program
	// does is tried on the very engine it meets in real use.
	if base := os.Getenv("CDP_BASE"); base != "" {
		useExisting(t, base)
		return base
	}
	bin := findChromium()
	// Where the browser tests are the point (CI), their absence is a failure.
	required := os.Getenv("CHROMIUM_REQUIRED") != ""
	if bin == "" {
		if required {
			t.Fatal("no Chromium/Chrome found, and CHROMIUM_REQUIRED is set")
		}
		t.Skip("no Chromium/Chrome found; set CHROMIUM_PATH to run browser tests")
	}
	// A browser that does not come up is started afresh: on a busy machine
	// (a CI runner at the wrong moment) one start in a great many hangs.
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		base, stop, err := launch(bin)
		if errors.Is(err, errExited) {
			t.Logf("attempt %d of %d: %v", attempt, attempts, err)
			continue
		}
		if err != nil {
			if required {
				t.Fatalf("cannot start %s: %v", bin, err)
			}
			t.Skipf("cannot start %s: %v", bin, err)
		}
		if base != "" {
			t.Cleanup(stop)
			return base
		}
		stop()
		t.Logf("Chromium did not expose a DevTools port (attempt %d of %d)", attempt, attempts)
	}
	t.Fatalf("Chromium did not expose a DevTools port in %d attempts (%s)", attempts, bin)
	return ""
}

// useExisting gets a browser that is already running ready for the tests:
// every page it shows is taken to an empty page.
func useExisting(t testing.TB, base string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		targets, err := cdp.Targets(ctx, base)
		done := 0
		if err == nil {
			for _, tg := range targets {
				if tg.Type != "page" {
					continue
				}
				p, perr := cdp.OpenPage(ctx, tg)
				if perr != nil {
					err = perr
					continue
				}
				if nerr := p.Navigate(ctx, "about:blank"); nerr != nil {
					err = nerr
				} else {
					done++
				}
				_ = p.Close()
			}
		}
		cancel()
		if done > 0 {
			t.Logf("using the browser at %s (%d page(s) taken to an empty page)", base, done)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the browser at %s (CDP_BASE) has no page that can be used: %v", base, err)
		}
		time.Sleep(time.Second)
	}
}

var errExited = errors.New("the browser exited at once")

// launch starts the browser and waits for its DevTools endpoint. base is
// empty if it did not come up in time; stop ends the browser and removes
// its profile either way.
func launch(bin string) (base string, stop func(), err error) {
	// Not a test's TempDir: Chromium's helper processes keep writing for a
	// moment after the main process is killed, which would fail the test
	// on cleanup.
	profile, err := os.MkdirTemp("", "cdptest-")
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(bin,
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile,
		"--no-sandbox",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-dev-shm-usage",
		// Nothing of the browser's own may reach out or run beside the
		// tests: no updates, no sync, no extensions, no crash uploads.
		"--disable-component-update",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-extensions",
		"--disable-breakpad",
		"--window-size=1280,900",
		"about:blank",
	)
	var stderr tail
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return "", nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop = func() {
		if runtime.GOOS == "windows" {
			// Chrome's helper processes outlive a plain kill there.
			_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
		for i := 0; i < 20; i++ {
			if os.RemoveAll(profile) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	portFile := filepath.Join(profile, "DevToolsActivePort")
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			// It will not come up any more; say what it said.
			stop()
			return "", nil, fmt.Errorf("%w: %s", errExited, stderr.String())
		default:
		}
		if data, err := os.ReadFile(portFile); err == nil {
			line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
			if port, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && port > 0 {
				b := cdp.Base(port)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := cdp.Version(ctx, b)
				cancel()
				if err == nil {
					return b, stop, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", stop, nil
}

// tail keeps the last few kilobytes of what a process wrote.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// FirstPage returns the first page target of the browser.
func FirstPage(t testing.TB, base string) cdp.Target {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		targets, err := cdp.Targets(ctx, base)
		cancel()
		if err == nil {
			for _, tg := range targets {
				if tg.Type == "page" {
					return tg
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no page target: %v (%s)", err, fmt.Sprint(targets))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
