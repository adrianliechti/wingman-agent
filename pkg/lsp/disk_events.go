package lsp

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fswatcher/fswatcher"
	"go.lsp.dev/protocol"
)

// One event reader owns the sets of files and directories it has observed.
type directoryWatch struct {
	*fswatcher.Watcher
	scope              watchDir
	session            *Session
	ctx                context.Context
	cancel             context.CancelFunc
	started            time.Time
	files, directories map[string]bool
}

func (w *directoryWatch) close() {
	w.cancel()
	w.Watcher.Close()
}

func (w *directoryWatch) run(real string) {
	for {
		select {
		case <-w.ctx.Done():
			return
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			slog.Warn("LSP file watch", "path", w.scope.path, "error", err)
		case e, ok := <-w.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(real, e.Name)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			if rel == "." {
				if e.Op.Has(fswatcher.Rename | fswatcher.Remove) {
					w.rewatch()
					return
				}
				continue
			}
			path := filepath.Join(w.scope.path, rel)
			w.event(path, e.Op)
			if w.scope.recursive && e.Op.Has(fswatcher.Create|fswatcher.Rename|fswatcher.Remove) {
				w.directoryChanged(path, e.Op)
			}
		}
	}
}

// A directory move may name only the directory, including when a populated
// tree moves into scope. Inspect that subtree once and keep watching it.
func (w *directoryWatch) directoryChanged(path string, op fswatcher.Op) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		w.removedChildren(path)
		return
	}
	if err != nil || !info.IsDir() || w.directories[path] || (!op.Has(fswatcher.Rename) && existingWrite(op, info, w.started)) {
		return
	}
	if runtime.GOOS == "linux" {
		// inotify needs an explicit watch when an existing tree moves in.
		if err := w.AddRecursive(path, diskWatchOps); err != nil && !errors.Is(err, fswatcher.ErrAlreadyAdded) {
			return
		}
	}
	_ = filepath.WalkDir(path, func(child string, entry fs.DirEntry, err error) error {
		if w.ctx.Err() != nil {
			return fs.SkipAll
		}
		if err == nil {
			if entry.IsDir() {
				w.directories[child] = true
			} else {
				w.event(child, fswatcher.Create)
			}
		}
		return nil
	})
}

func (w *directoryWatch) removedChildren(path string) {
	s := w.session
	prefix := path + string(filepath.Separator)
	// Open documents may predate this watch's first event.
	s.watchMu.Lock()
	if d := s.disk; d != nil {
		for file := range d.documents {
			if strings.HasPrefix(file, prefix) {
				w.files[file] = true
			}
		}
	}
	s.watchMu.Unlock()
	if w.directories[path] {
		delete(w.directories, path)
		for dir := range w.directories {
			if strings.HasPrefix(dir, prefix) {
				delete(w.directories, dir)
			}
		}
	}
	for file := range w.files {
		if strings.HasPrefix(file, prefix) {
			w.event(file, fswatcher.Remove)
			delete(w.files, file)
		}
	}
}

// A native watch follows its directory inode. After a rename or removal,
// subscribe again at the registered path or its nearest surviving parent.
func (w *directoryWatch) rewatch() {
	w.removedChildren(w.scope.path)
	s := w.session
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	d := s.disk
	if d == nil || d.active[w.scope] != w {
		return
	}
	delete(d.active, w.scope)
	w.close()
	if err := s.updateWatchesLocked(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("restore LSP file watch", "path", w.scope.path, "error", err)
	}
}

func (w *directoryWatch) event(path string, op fswatcher.Op) {
	s := w.session
	s.watchMu.Lock()
	d := s.disk
	interested := d != nil && d.active[w.scope] == w && (d.documents[path] || (withinDir(s.root, path) && s.watchKindLocked(path) != 0))
	s.watchMu.Unlock()
	if !interested {
		return
	}
	kind := protocol.FileChangeTypeChanged
	if op.Has(fswatcher.Create | fswatcher.Rename | fswatcher.Remove) {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			kind = protocol.FileChangeTypeDeleted
		case err != nil:
			return
		case !w.files[path] && !existingWrite(op, info, w.started):
			kind = protocol.FileChangeTypeCreated
		}
	}
	if kind == protocol.FileChangeTypeDeleted {
		delete(w.files, path)
	} else {
		w.files[path] = true
	}
	_ = s.FilesChanged(w.ctx, []FileChange{{Path: path, Type: kind}})
}
