// Package state remembers what has been done with each free offer, so that a
// game is claimed once, failed attempts back off, and nothing is retried
// after its promotion has ended.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status of an offer.
type Status string

const (
	// Pending offers have been seen but are not in the library yet.
	Pending Status = "pending"
	// Claimed offers were added to the library by this program.
	Claimed Status = "claimed"
	// Owned offers were already in the library when checked.
	Owned Status = "owned"
	// Skipped offers cannot be claimed from this PC (mobile-only, not
	// available in the account's region, needs a base game, ...).
	Skipped Status = "skipped"
)

// Done reports whether no further attempts are needed.
func (s Status) Done() bool { return s == Claimed || s == Owned || s == Skipped }

// Entry is the record of one offer.
type Entry struct {
	Title       string    `json:"title"`
	Status      Status    `json:"status"`
	FirstSeen   time.Time `json:"firstSeen"`
	Ends        time.Time `json:"ends"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	NextAttempt time.Time `json:"nextAttempt,omitzero"`
	LastResult  string    `json:"lastResult,omitempty"`
	DoneAt      time.Time `json:"doneAt,omitzero"`
}

type file struct {
	Version int               `json:"version"`
	Offers  map[string]*Entry `json:"offers"`
}

// Store is a JSON file of offer records. It is safe for concurrent use
// within one process.
type Store struct {
	path string
	mu   sync.Mutex
	data file
}

// Open loads the store at path, creating an empty one if it does not exist.
// A corrupt file is set aside (renamed to *.corrupt) rather than trusted:
// the worst outcome of an empty store is one redundant claim attempt, which
// the checkout answers with "already owned".
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: file{Version: 1, Offers: map[string]*Entry{}}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Offers == nil {
		_ = os.Rename(path, path+".corrupt")
		return s, nil
	}
	for k, e := range f.Offers {
		if e == nil {
			delete(f.Offers, k)
		}
	}
	f.Version = 1
	s.data = f
	return s, nil
}

// Get returns a copy of the record for key.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Offers[key]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// All returns a copy of every record.
func (s *Store) All() map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Entry, len(s.data.Offers))
	for k, e := range s.data.Offers {
		out[k] = *e
	}
	return out
}

// Update creates or modifies the record for key and saves the store.
func (s *Store) Update(key string, fn func(e *Entry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Offers[key]
	if !ok {
		e = &Entry{Status: Pending}
		s.data.Offers[key] = e
	}
	fn(e)
	return s.saveLocked()
}

// Prune drops records of promotions that ended long ago.
func (s *Store) Prune(now time.Time, keep time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, e := range s.data.Offers {
		if !e.Ends.IsZero() && now.Sub(e.Ends) > keep {
			delete(s.data.Offers, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
