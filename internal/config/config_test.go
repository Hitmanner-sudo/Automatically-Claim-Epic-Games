package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAndPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c != Default() {
		t.Fatalf("missing file should give defaults, got %+v", c)
	}
	if !c.RestartIdleLauncher || c.DryRun || c.CheckEveryMinutes != 60 {
		t.Fatalf("defaults changed: %+v", c)
	}

	// A file that sets only one thing keeps the other defaults (in
	// particular the ones that default to true).
	if err := os.WriteFile(path, []byte("\ufeff{\"country\": \" de \"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Country != "DE" || !c.RestartIdleLauncher || !c.KeepDiagnostics || c.CheckEveryMinutes != 60 {
		t.Fatalf("partial file: %+v", c)
	}
}

func TestNormalizeAndBadInput(t *testing.T) {
	c := Config{Country: "Germany", CheckEveryMinutes: 1}
	c.Normalize()
	if c.Country != "" || c.CheckEveryMinutes != 10 {
		t.Fatalf("normalize: %+v", c)
	}
	c = Config{Country: "u1", CheckEveryMinutes: 999999}
	c.Normalize()
	if c.Country != "" || c.CheckEveryMinutes != 1440 {
		t.Fatalf("normalize: %+v", c)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err == nil {
		t.Fatal("broken file should be reported")
	}
	// A file that cannot be read falls back to the cautious settings: it
	// was written because something other than the defaults was wanted.
	want := Default()
	want.DryRun, want.RestartIdleLauncher = true, false
	if c != want {
		t.Fatalf("broken file should fall back to the cautious settings, got %+v", c)
	}
	if !strings.Contains(err.Error(), "NOTHING WILL BE ORDERED") {
		t.Fatalf("error = %v", err)
	}

	// The slip Notepad users make most: a trailing comma.
	if err := os.WriteFile(path, []byte(`{"dryRun": true, "restartIdleLauncher": false,}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path); err == nil || !c.DryRun || c.RestartIdleLauncher {
		t.Fatalf("trailing comma: %+v, %v", c, err)
	}

	// One value of the wrong kind: the rest of the file still applies.
	if err := os.WriteFile(path, []byte(`{"dryRun": true, "checkEveryMinutes": "30", "restartIdleLauncher": false, "country": "de"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "checkEveryMinutes") {
		t.Fatalf("type error should name the setting: %v", err)
	}
	if !c.DryRun || c.RestartIdleLauncher || c.Country != "DE" || c.CheckEveryMinutes != 60 {
		t.Fatalf("type error: %+v", c)
	}
}

func TestWrongKindSafetySettingsAreReadCautiously(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for body, want := range map[string][2]bool{ // dryRun, restartIdleLauncher
		`{"dryRun": "true", "restartIdleLauncher": "false"}`: {true, false},
		`{"country": 5, "dryRun": "yes"}`:                    {true, true},
		`{"restartIdleLauncher": 0, "country": "us"}`:        {false, false},
		`{"DryRun": 1}`: {true, true},
		`{"checkEveryMinutes": "30", "dryRun": false}`: {false, true},
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err == nil {
			t.Errorf("%s: no error", body)
		}
		if c.DryRun != want[0] || c.RestartIdleLauncher != want[1] {
			t.Errorf("%s: dryRun=%v restartIdleLauncher=%v", body, c.DryRun, c.RestartIdleLauncher)
		}
	}
}

func TestNotepadEncodings(t *testing.T) {
	const text = `{"dryRun": true, "country": "fr"}`
	utf16le := []byte{0xFF, 0xFE}
	utf16be := []byte{0xFE, 0xFF}
	for _, r := range text {
		utf16le = append(utf16le, byte(r), 0)
		utf16be = append(utf16be, 0, byte(r))
	}
	for name, raw := range map[string][]byte{
		"UTF-8 with BOM": append([]byte{0xEF, 0xBB, 0xBF}, text...),
		"UTF-16 LE":      utf16le,
		"UTF-16 BE":      utf16be,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil || !c.DryRun || c.Country != "FR" || !c.RestartIdleLauncher {
			t.Errorf("%s: %+v, %v", name, c, err)
		}
	}
}

func TestEnsureFileAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	if err := EnsureFile(path); err != nil {
		t.Fatal(err)
	}
	want := Config{Country: "US", CheckEveryMinutes: 30, RestartIdleLauncher: false, DryRun: true, KeepDiagnostics: false, LauncherPath: `D:\Epic\EpicGamesLauncher.exe`}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFile(path); err != nil { // must not overwrite
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip: %+v != %+v", got, want)
	}
}
