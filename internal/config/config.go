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
	"unicode/utf16"
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
	raw = decodeText(raw)
	if err := json.Unmarshal(raw, &c); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			// One value is of the wrong kind; everything else in the file
			// was read and applies.
			c.Normalize()
			return c, fmt.Errorf("settings file %s: the value of %q is not of the expected kind and was ignored", path, typeErr.Field)
		}
		// The file exists because something other than the defaults was
		// wanted, and it cannot be read. The cautious reading is to order
		// nothing and to leave a running launcher alone until it is fixed.
		d := Default()
		d.DryRun, d.RestartIdleLauncher = true, false
		return d, fmt.Errorf("settings file %s is not valid JSON (%v); until it is fixed NOTHING WILL BE ORDERED (dry run)", path, err)
	}
	c.Normalize()
	return c, nil
}

// decodeText turns what Notepad may have saved (UTF-8 with a byte-order mark,
// or "Unicode", which is UTF-16) into plain UTF-8.
func decodeText(raw []byte) []byte {
	switch {
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		return raw[3:]
	case len(raw) >= 2 && (raw[0] == 0xFF && raw[1] == 0xFE || raw[0] == 0xFE && raw[1] == 0xFF):
		big := raw[0] == 0xFE
		units := make([]uint16, 0, len(raw)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			if big {
				units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
			} else {
				units = append(units, uint16(raw[i+1])<<8|uint16(raw[i]))
			}
		}
		return []byte(string(utf16.Decode(units)))
	}
	return raw
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
