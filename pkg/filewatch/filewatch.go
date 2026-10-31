// Package filewatch reports which files under a directory changed.
package filewatch

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
)

type ChangeType uint8

// The values match the LSP FileChangeType.
const (
	Created ChangeType = iota + 1
	Changed
	Deleted
)

type Change struct {
	// Path is slash-separated and relative to the scanned file system.
	Path string
	Type ChangeType
}

// A Source reports the changes since its previous Changes call. The polling
// Scanner implements it; an OS notification watcher could replace it.
type Source interface {
	Changes(ctx context.Context) ([]Change, error)
}

// ErrLimit reports a walk that exceeded Options.Limit.
var ErrLimit = errors.New("filewatch: too many entries")

type Options struct {
	// Recursive descends below the root's direct entries.
	Recursive bool
	// Skip leaves out an entry, and for a directory everything below it.
	// The path is relative to the root.
	Skip func(rel string, entry fs.DirEntry) bool
	// Track selects the entries that are compared; nil tracks all of them.
	Track func(rel string, entry fs.DirEntry) bool
	// Limit bounds the walked entries; zero is unlimited.
	Limit int
}

// Scanner is a Source that compares size, modification time and mode
// between walks of a directory.
type Scanner struct {
	fsys    fs.FS
	root    string
	opts    Options
	mu      sync.Mutex
	files   map[string]stamp
	scanned bool
}

type stamp struct {
	size     int64
	modified int64
	mode     fs.FileMode
}

func NewScanner(fsys fs.FS, root string, opts Options) *Scanner {
	return &Scanner{fsys: fsys, root: root, opts: opts}
}

// Changes walks the root and reports what changed since the previous call,
// sorted by path. The first call records a baseline and reports nothing; a
// failed walk keeps the previous baseline.
func (s *Scanner) Changes(ctx context.Context) ([]Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.walk(ctx)
	if err != nil {
		return nil, err
	}
	previous, scanned := s.files, s.scanned
	s.files, s.scanned = files, true
	if !scanned {
		return nil, nil
	}
	changes := []Change{}
	for name, current := range files {
		if old, exists := previous[name]; !exists {
			changes = append(changes, Change{Path: name, Type: Created})
		} else if old != current {
			changes = append(changes, Change{Path: name, Type: Changed})
		}
	}
	for name := range previous {
		if _, exists := files[name]; !exists {
			changes = append(changes, Change{Path: name, Type: Deleted})
		}
	}
	slices.SortFunc(changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return changes, nil
}

func (s *Scanner) walk(ctx context.Context) (map[string]stamp, error) {
	files := map[string]stamp{}
	visited := 0
	err := fs.WalkDir(s.fsys, s.root, func(name string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel := "."
		if name != s.root {
			rel = strings.TrimPrefix(name, s.root+"/")
			if s.root == "." {
				rel = name
			}
		}
		if name != s.root && s.opts.Skip != nil && s.opts.Skip(rel, entry) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if visited++; s.opts.Limit > 0 && visited > s.opts.Limit {
			return ErrLimit
		}
		if s.opts.Track == nil || s.opts.Track(rel, entry) {
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			files[name] = stamp{size: info.Size(), modified: info.ModTime().UnixNano(), mode: info.Mode()}
		}
		if !s.opts.Recursive && entry.IsDir() && name != s.root {
			return fs.SkipDir
		}
		return nil
	})
	return files, err
}
