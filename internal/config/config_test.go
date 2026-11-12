package config

import (
	"os"
	"path/filepath"
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
	if c != Default() {
		t.Fatalf("broken file should fall back to defaults, got %+v", c)
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
