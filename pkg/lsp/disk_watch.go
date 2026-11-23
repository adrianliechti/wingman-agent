package lsp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/fswatcher/fswatcher"
)

// diskWatches lives with the server connection. Its maps are protected by
// Session.watchMu. A recursive subscription also covers the open files below it.
type diskWatches struct {
	ctx       context.Context
	realRoot  string
	documents map[string]bool // open disk-backed paths
	active    map[watchDir]*directoryWatch
}

const diskWatchOps = fswatcher.Create | fswatcher.Write | fswatcher.Remove | fswatcher.Rename

type watchDir struct {
	path      string
	recursive bool
}

func newDiskWatches(ctx context.Context, root string) *diskWatches {
	real, _ := pathutil.Resolve(root)
	return &diskWatches{ctx: ctx, realRoot: real, documents: make(map[string]bool), active: make(map[watchDir]*directoryWatch)}
}

// Some servers resolve a symlinked workspace before registering their globs.
func (s *Session) projectPath(path string) string {
	if d := s.disk; d != nil && s.root != d.realRoot {
		if rel, err := filepath.Rel(d.realRoot, path); err == nil && filepath.IsLocal(rel) {
			return filepath.Join(s.root, rel)
		}
	}
	return path
}

func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

func (w fileWatcher) directory(root string) watchDir {
	dir, pattern := doublestar.SplitPattern(w.pattern)
	dir = filepath.FromSlash(dir)
	if !filepath.IsAbs(dir) {
		base := w.base
		if base == "" {
			base = root
		}
		dir = filepath.Join(base, dir)
	}
	recursive := strings.Contains(pattern, "/") || strings.Contains(pattern, "**")
	// A glob rooted above the workspace can still match files inside it.
	if !withinDir(root, dir) {
		if !withinDir(dir, root) {
			return watchDir{}
		}
		dir = root
	}
	// A server can register before its generated source directory exists.
	for dir != root {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			break
		}
		dir = filepath.Dir(dir)
		recursive = true
	}
	return watchDir{dir, recursive}
}

func (s *Session) watchDiskDocument(path string, watch bool) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	d := s.disk
	if d == nil {
		return
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if d.documents[path] == watch {
		return
	}
	if watch {
		d.documents[path] = true
		// An already watched parent covers another document in that directory.
		if d.active[watchDir{path: filepath.Dir(path)}] != nil {
			return
		}
	} else {
		delete(d.documents, path)
	}
	if err := s.updateWatchesLocked(); err != nil {
		slog.Debug("watch LSP document directory", "path", path, "error", err)
	}
}

func (s *Session) updateWatchesLocked() error {
	d := s.disk
	if d == nil || d.ctx.Err() != nil {
		return nil
	}
	dirs := make(map[string]bool, len(d.documents))
	for path := range d.documents {
		dirs[filepath.Dir(path)] = false
	}
	for _, watchers := range s.watchers {
		for _, w := range watchers {
			scope := w.directory(s.root)
			if scope.path != "" {
				dirs[scope.path] = dirs[scope.path] || scope.recursive
			}
		}
	}

	needed := make(map[watchDir]bool)
nextDir:
	for path, recursive := range dirs {
		for p := path; p != filepath.Dir(p); {
			p = filepath.Dir(p)
			if dirs[p] {
				continue nextDir
			}
		}
		needed[watchDir{path, recursive}] = true
	}

	// Install new watches before retiring the directories they replace.
	var added []watchDir
	for dir := range needed {
		if d.active[dir] != nil {
			continue
		}
		if err := s.startWatch(dir); err != nil {
			for _, dir := range added {
				d.active[dir].close()
				delete(d.active, dir)
			}
			return err
		}
		if d.active[dir] != nil {
			added = append(added, dir)
		}
	}
	for dir, w := range d.active {
		if !needed[dir] {
			w.close()
			delete(d.active, dir)
		}
	}
	return nil
}

func (s *Session) startWatch(dir watchDir) error {
	d := s.disk
	real, err := pathutil.Resolve(dir.path)
	if err != nil {
		return err
	}
	if withinDir(s.root, dir.path) && !withinDir(d.realRoot, real) {
		return nil
	}
	w, err := fswatcher.NewWatcher()
	if err != nil {
		return err
	}
	add := w.Add
	if dir.recursive {
		add = w.AddRecursive
	}
	started := time.Now()
	if err := add(real, diskWatchOps); err != nil {
		w.Close()
		return err
	}
	ctx, cancel := context.WithCancel(d.ctx)
	sub := &directoryWatch{
		Watcher: w, scope: dir, session: s, ctx: ctx, cancel: cancel, started: started,
		files: make(map[string]bool), directories: make(map[string]bool),
	}
	d.active[dir] = sub
	go sub.run(real)
	return nil
}

func (s *Session) stopDiskWatches() {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if s.disk != nil {
		for _, w := range s.disk.active {
			w.close()
		}
		s.disk = nil
	}
}
