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
