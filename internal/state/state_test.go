package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTripAndPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	if err := s.Update("ns:a", func(e *Entry) {
		e.Title = "A"
		e.Status = Claimed
		e.Ends = now.Add(-100 * 24 * time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("ns:b", func(e *Entry) {
		if e.Status != Pending {
			t.Errorf("new entries start pending, got %q", e.Status)
		}
		e.Title = "B"
		e.Attempts++
		e.Ends = now.Add(24 * time.Hour)
	}); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := again.Get("ns:a")
	if !ok || a.Status != Claimed || !a.Status.Done() {
		t.Fatalf("a = %+v %v", a, ok)
	}
	b, _ := again.Get("ns:b")
	if b.Attempts != 1 || b.Status.Done() {
		t.Fatalf("b = %+v", b)
	}

	if err := again.Prune(now, 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("ns:a"); ok {
		t.Fatal("old entry should have been pruned")
	}
	if len(again.All()) != 1 {
		t.Fatalf("all = %v", again.All())
	}
	final, _ := Open(path)
	if len(final.All()) != 1 {
		t.Fatalf("prune was not saved: %v", final.All())
	}
}

func TestCorruptFileIsSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.All()) != 0 {
		t.Fatal("expected an empty store")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file was not kept: %v", err)
	}
	if err := s.Update("k", func(e *Entry) { e.Title = "x" }); err != nil {
		t.Fatal(err)
	}
}
