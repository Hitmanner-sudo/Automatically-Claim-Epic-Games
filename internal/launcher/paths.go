// Package launcher finds, starts and stops the Epic Games Launcher.
package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ExeName is the launcher's main executable.
const ExeName = "EpicGamesLauncher.exe"

// exeFromCommand extracts the executable path from a shell command line such
// as `"C:\...\EpicGamesLauncher.exe" %1` or `C:\...\EpicGamesLauncher.exe -silent`.
func exeFromCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	if cmd[0] == '"' {
		if end := strings.IndexByte(cmd[1:], '"'); end >= 0 {
			return cmd[1 : 1+end]
		}
		return ""
	}
	lower := strings.ToLower(cmd)
	if i := strings.Index(lower, ".exe"); i >= 0 {
		return cmd[:i+4]
	}
	return ""
}

// isLauncherExe reports whether path names the launcher's executable.
func isLauncherExe(path string) bool {
	return strings.EqualFold(filepath.Base(strings.ReplaceAll(path, `\`, string(filepath.Separator))), ExeName) ||
		strings.HasSuffix(strings.ToLower(path), `\`+strings.ToLower(ExeName))
}

// exeUnder returns the launcher executables that may exist below an install
// root such as `C:\Program Files (x86)\Epic Games`, best first.
func exeUnder(root string) []string {
	root = strings.TrimRight(strings.TrimSpace(root), `\/`)
	if root == "" {
		return nil
	}
	return []string{
		root + `\Launcher\Portal\Binaries\Win64\` + ExeName,
		root + `\Launcher\Portal\Binaries\Win32\` + ExeName,
		root + `\Portal\Binaries\Win64\` + ExeName,
		root + `\Portal\Binaries\Win32\` + ExeName,
	}
}

// gameDirsFromManifests reads the launcher's install manifests and returns
// the folders of the games installed through it.
func gameDirsFromManifests(manifestDir string) []string {
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".item") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(manifestDir, e.Name()))
		if err != nil {
			continue
		}
		var m struct {
			InstallLocation string `json:"InstallLocation"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if loc := strings.TrimRight(strings.TrimSpace(m.InstallLocation), `\/`); len(loc) > 3 {
			out = append(out, loc)
		}
	}
	return out
}

// pathWithin reports whether file lies inside dir (Windows path rules:
// case-insensitive, either slash).
func pathWithin(file, dir string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.TrimRight(strings.ReplaceAll(s, "/", `\`), `\`))
	}
	f, d := norm(file), norm(dir)
	return d != "" && strings.HasPrefix(f, d+`\`)
}

// Generation tells the two launcher products apart.
type Generation int

const (
	// Legacy is the classic launcher (built on Unreal Engine, with an
	// embedded Chromium). It is what Epic's regular installer installs.
	Legacy Generation = iota
	// Preview is the new-generation launcher (built on WebView2) that Epic
	// started offering as an opt-in preview in late 2026.
	Preview
)

func (g Generation) String() string {
	if g == Preview {
		return "new-generation launcher"
	}
	return "classic launcher"
}

// generationOf tells the launcher generation from where its executable is.
func generationOf(exe string) Generation {
	p := strings.ToLower(strings.ReplaceAll(exe, "/", `\`))
	if strings.Contains(p, `\portal\binaries\`) {
		return Legacy
	}
	return Preview
}

// debugArgs returns what makes a launcher expose its embedded browser's
// DevTools endpoint on a localhost port: command-line arguments, extra
// environment, and the marker by which its processes can be recognised.
func debugArgs(gen Generation, port int) (args, env []string, flag string) {
	if gen == Preview {
		// WebView2 reads extra browser switches from this variable.
		return nil, []string{"WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=" + itoa(port)}, ""
	}
	// The classic launcher's web layer reads -cefdebug=<port>. -silent is
	// its start-in-tray mode: no main window until one is asked for.
	flag = "-cefdebug=" + itoa(port)
	return []string{flag, "-silent"}, nil, flag
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// splitCommandLine splits a Windows command line into its arguments
// (quotes group, a doubled quote inside quotes is a literal quote).
func splitCommandLine(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes, has := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			if inQuotes && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i++
			} else {
				inQuotes = !inQuotes
			}
			has = true
		case (c == ' ' || c == '\t') && !inQuotes:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

// userArgs extracts, from the command line of a launcher start made by the
// user (or by Windows at login), the arguments worth repeating when that
// start is redone: a com.epicgames.launcher:// link from a game shortcut,
// and the start-in-tray switches of the login entry.
func userArgs(commandLine string) []string {
	parts := splitCommandLine(commandLine)
	var out []string
	for i, a := range parts {
		if i == 0 {
			continue // the executable itself
		}
		low := strings.ToLower(a)
		switch {
		case strings.HasPrefix(low, "com.epicgames.launcher:"), strings.HasPrefix(low, "-com.epicgames.launcher:"):
			out = append(out, a)
		case low == "-silent", strings.HasPrefix(low, "-launchcontext="):
			out = append(out, a)
		}
	}
	return out
}
