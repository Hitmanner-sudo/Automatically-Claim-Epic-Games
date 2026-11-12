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
