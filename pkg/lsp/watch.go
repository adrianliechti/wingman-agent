package lsp

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/bmatcuk/doublestar/v4"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
)

// FileChange is a change to a file on disk, by absolute path.
type FileChange struct {
	Path string
	Type protocol.FileChangeType
}

type fileWatcher struct {
	base    string // directory of a relative pattern; empty for a plain glob
	pattern string
	kind    protocol.WatchKind
}

const watchedFilesMethod = "workspace/didChangeWatchedFiles"

// Only file watchers are registered dynamically; other registrations are
// accepted and ignored, as the client claims no dynamic support for them.
func (c *sessionClient) RegisterCapability(_ context.Context, params *protocol.RegistrationParams) error {
	s := c.session
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	next := make(map[string][]fileWatcher, len(s.watchers))
	maps.Copy(next, s.watchers)
	for _, registration := range params.Registrations {
		if registration.Method != watchedFilesMethod {
			continue
		}
		var options protocol.DidChangeWatchedFilesRegistrationOptions
		if err := protocol.Unmarshal(registration.RegisterOptions, &options); err != nil {
			return err
		}
		watchers := []fileWatcher{}
		for _, watcher := range options.Watchers {
			kind := watcher.Kind
			if kind == 0 {
				kind = protocol.WatchKindCreate | protocol.WatchKindChange | protocol.WatchKindDelete
			}
			switch pattern := watcher.GlobPattern.(type) {
			case protocol.Pattern:
				glob := string(pattern)
				if filepath.IsAbs(filepath.FromSlash(glob)) {
					glob = filepath.ToSlash(s.projectPath(filepath.FromSlash(glob)))
				}
				watchers = append(watchers, fileWatcher{pattern: glob, kind: kind})
			case *protocol.RelativePattern:
				var base string
				switch uri := pattern.BaseURI.(type) {
				case protocol.URI:
					base, _ = fileuri.Path(string(uri))
				case *protocol.WorkspaceFolder:
					base, _ = fileuri.Path(uri.URI.String())
				}
				if base != "" {
					base = s.projectPath(base)
					watchers = append(watchers, fileWatcher{base: base, pattern: string(pattern.Pattern), kind: kind})
				}
			}
		}
		next[registration.ID] = watchers
	}
	return s.setWatchersLocked(next)
}

func (c *sessionClient) UnregisterCapability(_ context.Context, params *protocol.UnregistrationParams) error {
	s := c.session
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	next := maps.Clone(s.watchers)
	for _, registration := range params.Unregisterations {
		if registration.Method == watchedFilesMethod {
			delete(next, registration.ID)
		}
	}
	return s.setWatchersLocked(next)
}

func (s *Session) setWatchersLocked(next map[string][]fileWatcher) error {
	previous := s.watchers
	s.watchers = next
	if err := s.updateWatchesLocked(); err != nil {
		s.watchers = previous
		return err
	}
	return nil
}

// FilesChanged refreshes documents opened from disk, which a server must not
// read itself while they are open, and reports the changes its watchers match.
func (s *Session) FilesChanged(ctx context.Context, changes []FileChange) error {
	for _, change := range changes {
		s.refreshDiskDocument(ctx, change.Path)
	}
	events := s.watchedEvents(changes)
	if len(events) == 0 {
		return nil
	}
	return s.rpc.DidChangeWatchedFiles(ctx, &protocol.DidChangeWatchedFilesParams{Changes: events})
}

func (s *Session) refreshDiskDocument(ctx context.Context, path string) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	s.mu.Lock()
	doc := s.documents[fileuri.FromPath(path)]
	diskBacked := doc.diskBacked()
	s.mu.Unlock()
	if !diskBacked {
		return
	}
	// A queued deletion may already have been followed by an atomic replacement.
	if _, err := s.openDocumentLocked(ctx, path); errors.Is(err, os.ErrNotExist) {
		_ = s.closeDocumentLocked(ctx, path)
	}
}

func (s *Session) watchedEvents(changes []FileChange) []protocol.FileEvent {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	var events []protocol.FileEvent
	for _, change := range changes {
		kind := protocol.WatchKind(1) << (change.Type - 1)
		if s.watchKindLocked(change.Path)&kind != 0 {
			events = append(events, protocol.FileEvent{URI: lspuri.MustParse(fileuri.FromPath(change.Path)), Type: change.Type})
		}
	}
	return events
}

func (s *Session) watchKindLocked(path string) protocol.WatchKind {
	var kind protocol.WatchKind
	for _, registered := range s.watchers {
		for _, watcher := range registered {
			if watcher.matches(path, s.root) {
				kind |= watcher.kind
			}
		}
	}
	return kind
}

// A plain glob may be written for the absolute path or for the path within
// the project, as clients differ; a relative pattern applies below its base.
func (w fileWatcher) matches(path, root string) bool {
	base := w.base
	if base == "" {
		absolute := strings.TrimPrefix(filepath.ToSlash(path), "/")
		if matched, _ := doublestar.Match(strings.TrimPrefix(w.pattern, "/"), absolute); matched {
			return true
		}
		base = root
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	matched, _ := doublestar.Match(w.pattern, filepath.ToSlash(rel))
	return matched
}
