package fs

import (
	"crypto/sha256"
	"sync"
)

// contentTracker records the exact bytes read or written this session so the
// write tool can refuse to overwrite file content the model has never seen.
// Keying by content instead of path sidesteps symlink/case aliasing and makes
// staleness detection byte-exact. A nil tracker disables the guard.
type contentTracker struct {
	mu   sync.Mutex
	seen map[[sha256.Size]byte]struct{}
}

func newContentTracker() *contentTracker {
	return &contentTracker{seen: map[[sha256.Size]byte]struct{}{}}
}

func (t *contentTracker) record(content []byte) {
	if t == nil {
		return
	}
	key := sha256.Sum256(content)
	t.mu.Lock()
	t.seen[key] = struct{}{}
	t.mu.Unlock()
}

func (t *contentTracker) knows(content []byte) bool {
	if t == nil {
		return true
	}
	key := sha256.Sum256(content)
	t.mu.Lock()
	_, ok := t.seen[key]
	t.mu.Unlock()
	return ok
}
