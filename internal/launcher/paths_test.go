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

func TestGenerationOf(t *testing.T) {
	cases := map[string]Generation{
		`C:\Program Files\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe`:       Legacy,
		`C:\Program Files (x86)\Epic Games\Launcher\Portal\Binaries\Win32\EpicGamesLauncher.exe`: Legacy,
		`D:/Games/Epic/Launcher/Portal/Binaries/Win64/EpicGamesLauncher.exe`:                     Legacy,
		`C:\Users\me\AppData\Local\Epic Games\Launcher\EpicGamesLauncher.exe`:                    Preview,
	}
	for path, want := range cases {
		if got := generationOf(path); got != want {
			t.Errorf("generationOf(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDebugArgs(t *testing.T) {
	args, env, flag := debugArgs(Legacy, 51234)
	if len(args) != 2 || args[0] != "-cefdebug=51234" || args[1] != "-silent" || env != nil || flag != "-cefdebug=51234" {
		t.Errorf("legacy: %v %v %q", args, env, flag)
	}
	args, env, flag = debugArgs(Preview, 51234)
	if len(args) != 0 || len(env) != 1 || env[0] != "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=51234" || flag != "" {
		t.Errorf("preview: %v %v %q", args, env, flag)
	}
	for n, want := range map[int]string{0: "0", 7: "7", 65534: "65534", -12: "-12"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q", n, got)
		}
	}
}

func TestUserArgs(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{`"C:\Program Files\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe"`, nil},
		{`"C:\Program Files\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe" com.epicgames.launcher://apps/Fortnite?action=launch&silent=true`,
			[]string{"com.epicgames.launcher://apps/Fortnite?action=launch&silent=true"}},
		{`"C:\x\EpicGamesLauncher.exe" "com.epicgames.launcher://store/product/a b"`, []string{"com.epicgames.launcher://store/product/a b"}},
		{`"C:\x\EpicGamesLauncher.exe" -silent -launchcontext=boot`, []string{"-silent", "-launchcontext=boot"}},
		{`C:\x\EpicGamesLauncher.exe -opengl -SkipBuildPatchPrereq -com.epicgames.launcher://apps/abc?action=launch`, []string{"-com.epicgames.launcher://apps/abc?action=launch"}},
		// Internal switches of a re-executed launcher are not repeated.
		{`"C:\x\EpicGamesLauncher.exe" "C:\x\EpicGamesLauncher.exe" -cefdebug=9222 -AllowSoftwareRendering -SaveToUserDir -Messaging -ForcedRestart`, nil},
		{``, nil},
	}
	for _, c := range cases {
		got := userArgs(c.cmd)
		if len(got) != len(c.want) {
			t.Errorf("userArgs(%q) = %q, want %q", c.cmd, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("userArgs(%q) = %q, want %q", c.cmd, got, c.want)
			}
		}
	}
}

func TestSplitCommandLine(t *testing.T) {
	got := splitCommandLine(`"C:\Program Files\x.exe"  a "b c" d""e "say ""hi"""`)
	want := []string{`C:\Program Files\x.exe`, "a", "b c", "de", `say "hi"`}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestStoreLinks(t *testing.T) {
	got := StoreLinks("p", "flotsam-266287")
	if len(got) != 2 || got[0] != "com.epicgames.launcher://store/product/flotsam-266287" || got[1] != "com.epicgames.launcher://store/p/flotsam-266287" {
		t.Errorf("game links = %q", got)
	}
	got = StoreLinks("bundles", "lisa-the-definitive-edition")
	if len(got) != 2 || got[0] != "com.epicgames.launcher://store/bundles/lisa-the-definitive-edition" {
		t.Errorf("bundle links = %q", got)
	}
	if StoreLinks("p", "") != nil {
		t.Error("no slug, no links")
	}
}

func TestPendingInstall(t *testing.T) {
	dir := t.TempDir()
	if pendingInstall(dir) {
		t.Fatal("no Pending folder means nothing is pending")
	}
	pending := filepath.Join(dir, "Pending")
	if err := os.Mkdir(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if pendingInstall(dir) {
		t.Fatal("an empty Pending folder means nothing is pending")
	}
	if err := os.WriteFile(filepath.Join(pending, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pendingInstall(dir) {
		t.Fatal("only .item files count")
	}
	if err := os.WriteFile(filepath.Join(pending, "72A9391D4D5E46E802EDC78827BD18DA.item"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !pendingInstall(dir) {
		t.Fatal("an .item file in Pending is an unfinished install")
	}
}

func TestHasSwitch(t *testing.T) {
	const exe = `"C:\Program Files\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe"`
	for _, c := range []struct {
		cmd, sw string
		want    bool
	}{
		{exe + ` -cefdebug=5555 -silent`, "-cefdebug=5555", true},
		{exe + ` -silent -cefdebug=5555`, "-cefdebug=5555", true},
		{exe + ` "-cefdebug=5555" -silent`, "-cefdebug=5555", true},
		{exe + ` -cefdebug=55551 -silent`, "-cefdebug=5555", false},
		{exe + ` -cefdebug=15555 -silent`, "-cefdebug=5555", false},
		{exe + ` -xcefdebug=5555`, "-cefdebug=5555", false},
		{exe + ` -cefdebug=55551 -cefdebug=5555`, "-cefdebug=5555", true},
		{exe + ` -silent`, "-cefdebug=5555", false},
		{exe, "", false},
		{"", "-cefdebug=5555", false},
	} {
		if got := hasSwitch(c.cmd, c.sw); got != c.want {
			t.Errorf("hasSwitch(%q, %q) = %v, want %v", c.cmd, c.sw, got, c.want)
		}
	}
}
