// Package logx is the program's only output channel: a small, size-capped
// log file. Nothing is ever shown on screen.
package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger writes timestamped lines to a file and rotates it when it grows.
type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	limit    int64 // size at which the next rotation is tried
	keep     int
	f        *os.File
	size     int64
	mirror   io.Writer
}

// Open creates a logger writing to path. mirror (may be nil) receives a copy
// of every line, for interactive commands.
func Open(path string, mirror io.Writer) (*Logger, error) {
	l := &Logger{path: path, maxBytes: 1 << 20, keep: 3, mirror: mirror}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// Discard returns a logger that only writes to mirror (or nowhere).
func Discard(mirror io.Writer) *Logger { return &Logger{mirror: mirror} }

func (l *Logger) open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

func (l *Logger) rotate() {
	l.f.Close()
	l.f = nil
	// The current file is moved aside first. On Windows that fails while
	// another process has it open (a second command, or somebody following
	// the log); the older files are then left as they are and the next try
	// comes after the file has grown some more, not on every line.
	staging := l.path + ".rot"
	if err := os.Rename(l.path, staging); err != nil {
		_ = l.open()
		l.limit = l.size + l.maxBytes/16
		return
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	_ = os.Rename(staging, l.path+".1")
	l.limit = l.maxBytes
	_ = l.open()
}

// Printf writes one line.
func (l *Logger) Printf(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mirror != nil {
		_, _ = io.WriteString(l.mirror, line)
	}
	if l.path == "" {
		return
	}
	if l.f == nil {
		if l.open() != nil {
			return
		}
	}
	if l.limit == 0 {
		l.limit = l.maxBytes
	}
	if l.size+int64(len(line)) > l.limit {
		l.rotate()
		if l.f == nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

// Close flushes and closes the file.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
