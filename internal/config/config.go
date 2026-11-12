// Package config holds the few settings the program has. Every setting has
// a sensible default; the file exists so that defaults can be changed, not
// because it must be filled in.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the content of config.json.
type Config struct {
	// Country is the two-letter country of the Epic account, used to ask
	// Epic which games are free there. Empty means "use this PC's region".
	Country string `json:"country"`
	// CheckEveryMinutes is how often the free-games list is fetched. The
	// program also wakes up by itself when a new giveaway is due to start.
	CheckEveryMinutes int `json:"checkEveryMinutes"`
	// LauncherPath overrides auto-detection of EpicGamesLauncher.exe.
	LauncherPath string `json:"launcherPath"`
	// RestartIdleLauncher allows closing a launcher that is merely sitting
	// in the tray (no window open, no game running, no download) so that a
	// hidden one can run for a minute; it is put back in the tray afterwards.
	// When false the program waits until the launcher is not running at all.
	RestartIdleLauncher bool `json:"restartIdleLauncher"`
	// DryRun goes through everything except placing the order.
	DryRun bool `json:"dryRun"`
	// KeepDiagnostics saves a screenshot and details of attempts that did
	// not succeed (on this PC only), to help find out why.
	KeepDiagnostics bool `json:"keepDiagnostics"`
}

// Default returns the settings used when no file exists.
func Default() Config {
	return Config{
		CheckEveryMinutes:   60,
		RestartIdleLauncher: true,
		KeepDiagnostics:     true,
	}
}

// Normalize brings out-of-range values back to something safe.
func (c *Config) Normalize() {
	c.Country = strings.ToUpper(strings.TrimSpace(c.Country))
	if len(c.Country) != 2 || c.Country[0] < 'A' || c.Country[0] > 'Z' || c.Country[1] < 'A' || c.Country[1] > 'Z' {
		c.Country = ""
	}
	if c.CheckEveryMinutes < 10 {
		c.CheckEveryMinutes = 10
	}
	if c.CheckEveryMinutes > 24*60 {
		c.CheckEveryMinutes = 24 * 60
	}
	c.LauncherPath = strings.TrimSpace(c.LauncherPath)
}

// Load reads the file at path. A missing file yields the defaults. Fields
// absent from the file keep their defaults.
func Load(path string) (Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read settings: %w", err)
	}
	// Tolerate a UTF-8 byte-order mark: Notepad adds one.
	raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
	if err := json.Unmarshal(raw, &c); err != nil {
		d := Default()
		return d, fmt.Errorf("settings file %s is not valid JSON (%v); using defaults", path, err)
	}
	c.Normalize()
	return c, nil
}

// Save writes the settings.
func Save(path string, c Config) error {
	c.Normalize()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// EnsureFile writes the defaults to path if no file exists yet, so that
// there is something to edit.
func EnsureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return Save(path, Default())
}
