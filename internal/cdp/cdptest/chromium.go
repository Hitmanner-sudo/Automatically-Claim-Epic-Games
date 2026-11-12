// Package cdptest starts a real headless Chromium for integration tests.
package cdptest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
		"--window-size=1280,900",
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return "", nil, err
	}
	stop = func() {
		if runtime.GOOS == "windows" {
			// Chrome's helper processes outlive a plain kill there.
			_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
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
