// Package cdptest starts a real headless Chromium for integration tests.
package cdptest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if bin == "" {
		t.Skip("no Chromium/Chrome found; set CHROMIUM_PATH to run browser tests")
	}
	// Not t.TempDir(): Chromium's helper processes keep writing for a moment
	// after the main process is killed, which would fail the test on cleanup.
	profile, err := os.MkdirTemp("", "cdptest-")
	if err != nil {
		t.Fatal(err)
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
		t.Skipf("cannot start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		for i := 0; i < 20; i++ {
			if os.RemoveAll(profile) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	portFile := filepath.Join(profile, "DevToolsActivePort")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile); err == nil {
			line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
			if port, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && port > 0 {
				base := cdp.Base(port)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := cdp.Version(ctx, base)
				cancel()
				if err == nil {
					return base
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Chromium did not expose a DevTools port within 30s (%s)", bin)
	return ""
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
