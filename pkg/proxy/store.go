package proxy

import (
	"slices"
	"sync"

	"github.com/google/uuid"
)

const defaultMaxEntries = 100

type Store struct {
	mu         sync.RWMutex
	entries    []RequestEntry
	maxEntries int

	totalInput  int
	totalOutput int
}

func newStore() *Store {
	return &Store{
		maxEntries: defaultMaxEntries,
	}
}

func (s *Store) Add(entry RequestEntry) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry.ID = uuid.NewString()
	s.entries = append(s.entries, entry)

	s.totalInput += entry.InputTokens
	s.totalOutput += entry.OutputTokens

	// Plain re-slicing (s.entries = s.entries[k:]) would leave the dropped
	// entries pinned in the underlying array, preventing GC of their
	// RequestBody/ResponseBody buffers until the slice next grows.
	// slices.Delete zeros the freed slots so the byte buffers can be reclaimed.
	if excess := len(s.entries) - s.maxEntries; excess > 0 {
		s.entries = slices.Delete(s.entries, 0, excess)
	}

	return entry.ID
}

func (s *Store) List() []RequestEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]RequestEntry, len(s.entries))
	copy(result, s.entries)
	return result
}

func (s *Store) Get(id string) (RequestEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, e := range s.entries {
		if e.ID == id {
			return e, true
		}
	}

	return RequestEntry{}, false
}

func (s *Store) TotalTokens() (input, output int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.totalInput, s.totalOutput
}
