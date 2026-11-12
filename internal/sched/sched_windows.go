//go:build windows

package sched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// CurrentUserSID returns the security identifier of the user this program
// runs as.
func CurrentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("find out the current user: %w", err)
	}
	return tu.User.Sid.String(), nil
}

// schtasks runs Windows' own task tool without a window and returns what it
// printed.
func schtasks(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tool := "schtasks.exe"
	if dir, err := windows.GetSystemDirectory(); err == nil {
		tool = filepath.Join(dir, tool)
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if ctx.Err() != nil {
		return text, errors.New("the Task Scheduler did not answer in time")
	}
	if err != nil {
		if text != "" {
			// What the tool says is more useful than its exit code.
			return text, fmt.Errorf("%s (%v)", firstLine(text), err)
		}
		return text, err
	}
	return text, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Register creates the task, replacing one of the same name.
func Register(s Spec) error {
	f, err := os.CreateTemp("", "EpicAutoClaim-task-*.xml")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	_, werr := f.Write(UTF16File(s.XML()))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if _, err := schtasks("/Create", "/TN", s.Name, "/XML", path, "/F"); err != nil {
		return fmt.Errorf("create the hourly task: %w", err)
	}
	return nil
}

// Exists reports whether a task of that name is registered.
func Exists(name string) bool {
	_, err := schtasks("/Query", "/TN", name)
	return err == nil
}

// Definition returns the registered task's definition as XML, or "" if
// there is no such task.
func Definition(name string) string {
	out, err := schtasks("/Query", "/TN", name, "/XML")
	if err != nil {
		return ""
	}
	return out
}

// Remove deletes the task. A task that does not exist is not an error.
func Remove(name string) error {
	if !Exists(name) {
		return nil
	}
	if _, err := schtasks("/Delete", "/TN", name, "/F"); err != nil {
		return fmt.Errorf("remove the daily task: %w", err)
	}
	return nil
}
