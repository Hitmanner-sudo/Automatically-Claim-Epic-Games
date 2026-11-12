package launcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExeFromCommand(t *testing.T) {
	cases := map[string]string{
		`"C:\Program Files (x86)\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe" %1`: `C:\Program Files (x86)\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe`,
		`"D:\Epic\Launcher\Portal\Binaries\Win32\EpicGamesLauncher.exe" -silent`:                      `D:\Epic\Launcher\Portal\Binaries\Win32\EpicGamesLauncher.exe`,
		`C:\Epic\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe -silent`:                        `C:\Epic\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe`,
		`   `:                  ``,
		`"unterminated`:        ``,
		`not a program at all`: ``,
	}
	for in, want := range cases {
		if got := exeFromCommand(in); got != want {
			t.Errorf("exeFromCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLauncherExe(t *testing.T) {
	if !isLauncherExe(`C:\Program Files (x86)\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe`) {
		t.Error("full path not recognised")
	}
	if !isLauncherExe(`c:\x\epicgameslauncher.EXE`) {
		t.Error("case-insensitive match failed")
	}
	if isLauncherExe(`C:\x\EpicWebHelper.exe`) || isLauncherExe(`C:\x\NotEpicGamesLauncher.exe`) {
		t.Error("wrong executable accepted")
	}
}

func TestExeUnder(t *testing.T) {
	got := exeUnder(`C:\Program Files (x86)\Epic Games\`)
	if len(got) == 0 || got[0] != `C:\Program Files (x86)\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe` {
		t.Fatalf("got %v", got)
	}
	if exeUnder("  ") != nil {
		t.Fatal("empty root should give nothing")
	}
}

func TestGameDirsFromManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("A.item", `{"FormatVersion":0,"InstallLocation":"D:\\Games\\Flotsam","LaunchExecutable":"Flotsam.exe"}`)
	write("B.item", `{"InstallLocation":"C:\\Program Files\\Epic Games\\Fortnite\\"}`)
	write("C.item", `{broken`)
	write("D.item", `{"InstallLocation":"C:\\"}`) // a drive root must never count as a game folder
	write("notes.txt", `{"InstallLocation":"E:\\Nope"}`)
	got := gameDirsFromManifests(dir)
	if len(got) != 2 || got[0] != `D:\Games\Flotsam` || got[1] != `C:\Program Files\Epic Games\Fortnite` {
		t.Fatalf("got %q", got)
	}
	if gameDirsFromManifests(filepath.Join(dir, "missing")) != nil {
		t.Fatal("missing folder should give nothing")
	}
}

func TestPathWithin(t *testing.T) {
	if !pathWithin(`d:\games\flotsam\Flotsam.exe`, `D:\Games\Flotsam`) {
		t.Error("case-insensitive containment failed")
	}
	if !pathWithin(`D:/Games/Flotsam/bin/x.exe`, `D:\Games\Flotsam\`) {
		t.Error("slash-insensitive containment failed")
	}
	if pathWithin(`D:\Games\FlotsamTwo\x.exe`, `D:\Games\Flotsam`) {
		t.Error("sibling folder with a common prefix must not match")
	}
	if pathWithin(`D:\Games\Flotsam`, `D:\Games\Flotsam`) || pathWithin(`x`, ``) {
		t.Error("a folder does not contain itself; empty folder contains nothing")
	}
}
