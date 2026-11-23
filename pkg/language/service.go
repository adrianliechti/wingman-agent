package language

import (
	"context"
	"fmt"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/treesitter"
)

// Service owns the code-intelligence backends for one workspace. It is the
// only layer that chooses between language-server and structural results.
type Service struct {
	root    string
	manager *lsp.Manager
	syntax  *treesitter.Workspace

	lifeMu sync.RWMutex
	closed bool
}

func New(root string, servers ...lsp.Server) *Service {
	return &Service{
		root:    root,
		manager: lsp.NewManager(root, servers...),
		syntax:  treesitter.NewWorkspace(root),
	}
}

func (s *Service) Manager() *lsp.Manager { return s.manager }

func (s *Service) Close() {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.manager.Close()
}

func (s *Service) HasLSPFor(filePath string) bool {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	return !s.closed && s.manager.FindServer(filePath) != nil
}

// withLSPDocument keeps the session alive while synchronizing and querying a document.
func withLSPDocument[T any](ctx context.Context, s *Service, filePath string, content *string, request func(*lsp.Session, string) (T, error)) (T, error) {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	var zero T
	if s.closed {
		return zero, fmt.Errorf("code intelligence service is closed")
	}
	session, err := s.manager.GetSession(ctx, filePath)
	if err != nil {
		return zero, err
	}
	var uri string
	if content != nil {
		uri, err = session.SyncRequestDocument(ctx, filePath, *content)
	} else {
		uri, err = session.OpenDocument(ctx, filePath)
	}
	if err != nil {
		return zero, err
	}
	return request(session, uri)
}

func (s *Service) SyncDocument(ctx context.Context, filePath, content string, saved bool) error {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed || s.manager.FindServer(filePath) == nil {
		return nil
	}
	session, err := s.manager.GetSession(ctx, filePath)
	if err != nil {
		return err
	}
	_, err = session.SyncEditorDocument(ctx, filePath, content, saved)
	return err
}

func (s *Service) CloseDocument(ctx context.Context, filePath string) error {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return nil
	}
	return s.manager.CloseDocument(ctx, filePath)
}

func (s *Service) Capabilities(ctx context.Context, filePath string) (lsp.ServerCapabilities, bool, error) {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed || s.manager.FindServer(filePath) == nil {
		return lsp.ServerCapabilities{}, false, nil
	}
	session, err := s.manager.GetSession(ctx, filePath)
	if err != nil {
		return lsp.ServerCapabilities{}, false, err
	}
	return session.Capabilities(), true, nil
}

func (s *Service) DocumentContent(filePath string) (string, bool) {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return "", false
	}
	session, ok := s.manager.ActiveSession(filePath)
	if !ok {
		return "", false
	}
	return session.DocumentContent(filePath)
}

func (s *Service) Running() bool {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	return !s.closed && s.manager.Running()
}

func (s *Service) FilesChanged(ctx context.Context, changes []lsp.FileChange) {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if !s.closed {
		s.manager.FilesChanged(ctx, changes)
	}
}

// VirtualContent reads a virtual document a language server returned.
func (s *Service) VirtualContent(ctx context.Context, uri string) (string, error) {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return "", fmt.Errorf("code intelligence service is closed")
	}
	return s.manager.VirtualContent(ctx, uri)
}
