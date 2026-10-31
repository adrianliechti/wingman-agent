package lsp

import (
	"context"
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
				watchers = append(watchers, fileWatcher{pattern: string(pattern), kind: kind})
			case *protocol.RelativePattern:
				var base string
				switch uri := pattern.BaseURI.(type) {
				case protocol.URI:
					base, _ = fileuri.Path(string(uri))
				case *protocol.WorkspaceFolder:
					base, _ = fileuri.Path(uri.URI.String())
				}
				if base != "" {
					watchers = append(watchers, fileWatcher{base: base, pattern: string(pattern.Pattern), kind: kind})
				}
			}
		}
		c.session.watchMu.Lock()
		if c.session.watchers == nil {
			c.session.watchers = map[string][]fileWatcher{}
		}
		c.session.watchers[registration.ID] = watchers
		c.session.watchMu.Unlock()
	}
	return nil
}

func (c *sessionClient) UnregisterCapability(_ context.Context, params *protocol.UnregistrationParams) error {
	c.session.watchMu.Lock()
	defer c.session.watchMu.Unlock()
	for _, registration := range params.Unregisterations {
		delete(c.session.watchers, registration.ID)
	}
	return nil
}

// FilesChanged refreshes documents opened from disk, which a server must not
// read itself while they are open, and reports the changes its watchers match.
func (s *Session) FilesChanged(ctx context.Context, changes []FileChange) error {
	for _, change := range changes {
		s.refreshDiskDocument(ctx, change)
	}
	events := s.watchedEvents(changes)
	if len(events) == 0 {
		return nil
	}
	return s.rpc.DidChangeWatchedFiles(ctx, &protocol.DidChangeWatchedFilesParams{Changes: events})
}

func (s *Session) refreshDiskDocument(ctx context.Context, change FileChange) {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	s.mu.Lock()
	doc := s.documents[fileuri.FromPath(change.Path)]
	diskBacked := doc != nil && doc.opened && doc.saved && !doc.editor
	s.mu.Unlock()
	if !diskBacked {
		return
	}
	if change.Type == protocol.FileChangeTypeDeleted {
		_ = s.closeDocumentLocked(ctx, change.Path)
	} else {
		_, _ = s.openDocumentLocked(ctx, change.Path)
	}
}

func (s *Session) watchedEvents(changes []FileChange) []protocol.FileEvent {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	var events []protocol.FileEvent
	for _, change := range changes {
		kind := protocol.WatchKind(1) << (change.Type - 1)
	watchers:
		for _, registered := range s.watchers {
			for _, watcher := range registered {
				if watcher.kind&kind != 0 && watcher.matches(change.Path, s.rootURI) {
					events = append(events, protocol.FileEvent{URI: lspuri.MustParse(fileuri.FromPath(change.Path)), Type: change.Type})
					break watchers
				}
			}
		}
	}
	return events
}

// A plain glob may be written for the absolute path or for the path within
// the project, as clients differ; a relative pattern applies below its base.
func (w fileWatcher) matches(path, rootURI string) bool {
	if w.base != "" {
		rel, err := filepath.Rel(w.base, path)
		if err != nil || !filepath.IsLocal(rel) {
			return false
		}
		matched, _ := doublestar.Match(w.pattern, filepath.ToSlash(rel))
		return matched
	}
	absolute := strings.TrimPrefix(filepath.ToSlash(path), "/")
	if matched, _ := doublestar.Match(strings.TrimPrefix(w.pattern, "/"), absolute); matched {
		return true
	}
	if root, ok := fileuri.Path(rootURI); ok {
		if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
			matched, _ := doublestar.Match(w.pattern, filepath.ToSlash(rel))
			return matched
		}
	}
	return false
}
