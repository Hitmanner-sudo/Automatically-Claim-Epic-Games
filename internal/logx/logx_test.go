package logx

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationKeepsBoundedHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "app.log")
	var mirror bytes.Buffer
	l, err := Open(path, &mirror)
	if err != nil {
		t.Fatal(err)
	}
	l.maxBytes = 400
	for i := 0; i < 200; i++ {
		l.Printf("line %03d %s", i, strings.Repeat("x", 20))
	}
	l.Close()

	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 4 { // app.log + .1 .2 .3
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("files = %v", names)
	}
	cur, _ := os.ReadFile(path)
	if !strings.Contains(string(cur), "line 199") || len(cur) > 400 {
		t.Fatalf("current log wrong (len %d): %q", len(cur), cur)
	}
	if !strings.Contains(mirror.String(), "line 000") || !strings.Contains(mirror.String(), "line 199") {
		t.Fatal("mirror did not receive every line")
	}
}

func TestDiscard(t *testing.T) {
	var mirror bytes.Buffer
	l := Discard(&mirror)
	l.Printf("hello %d", 1)
	l.Close()
	if !strings.Contains(mirror.String(), "hello 1") {
		t.Fatalf("mirror = %q", mirror.String())
	}
	Discard(nil).Printf("nowhere")
}

func TestRotationThatCannotMoveTheFileLeavesHistoryAlone(t *testing.T) {
	// On Windows the current file cannot be renamed while another process
	// has it open. Stand-in for that here: the name it would be moved to is
	// taken by a directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	for i, old := range []string{"newest", "middle", "oldest"} {
		if err := os.WriteFile(path+"."+string(rune('1'+i)), []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(path+".rot", "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.maxBytes = 800
	for i := 0; i < 100; i++ {
		l.Printf("line %03d %s", i, strings.Repeat("x", 20))
	}
	// Nothing was lost: every line is in the current file, and the older
	// files are what they were.
	cur, _ := os.ReadFile(path)
	if !strings.Contains(string(cur), "line 000") || !strings.Contains(string(cur), "line 099") {
		t.Fatalf("lines were lost (len %d)", len(cur))
	}
	for i, old := range []string{"newest", "middle", "oldest"} {
		got, _ := os.ReadFile(path + "." + string(rune('1'+i)))
		if string(got) != old {
			t.Fatalf("history file %d was disturbed: %q", i+1, got)
		}
	}
	// The retry is spaced out, not made on every line.
	if l.limit <= l.maxBytes {
		t.Fatalf("limit = %d", l.limit)
	}
	// Once the obstacle is gone, rotation resumes.
	if err := os.RemoveAll(path + ".rot"); err != nil {
		t.Fatal(err)
	}
	for i := 100; i < 110; i++ {
		l.Printf("line %03d %s", i, strings.Repeat("x", 20))
	}
	l.Close()
	first, _ := os.ReadFile(path + ".1")
	if !strings.Contains(string(first), "line 000") {
		t.Fatalf("rotation did not resume: .1 = %.60q", first)
	}
	second, _ := os.ReadFile(path + ".2")
	if string(second) != "newest" {
		t.Fatalf(".2 = %q", second)
	}
	cur, _ = os.ReadFile(path)
	if len(cur) > 800 || !strings.Contains(string(cur), "line 109") {
		t.Fatalf("current log wrong (len %d)", len(cur))
	}
}
