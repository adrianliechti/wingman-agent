package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Manager starts a language server the first time a file it handles is asked about.
type Manager struct {
	workingDir string
	lifecycle  context.Context
	cancel     context.CancelFunc
	connect    sessionConnector
	lookPath   func(string) string
	servers    []Server // in order of registration

	mu       sync.Mutex
	sessions map[string]*sessionEntry // by server name
	restarts map[string]int
	closed   bool

	commandMu sync.Mutex
	commands  map[string]foundCommand
}

type foundCommand struct {
	path string
	at   time.Time
}

// sessionEntry is shared by every caller waiting for a server to start. The
// result is published by closing done; failed entries are removed so later
// calls retry.
type sessionEntry struct {
	done    chan struct{}
	session *Session
	err     error
	closed  []string // documents closed while the session starts
}

type sessionConnector func(context.Context, string, Server) (*Session, error)

const commandCacheTTL = 30 * time.Second

// NewManager gives registered servers precedence over the built-ins.
func NewManager(workingDir string, servers ...Server) *Manager {
	lifecycle, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		workingDir: workingDir,
		lifecycle:  lifecycle,
		cancel:     cancel,
		connect:    connect,
		lookPath:   CommandLookup,
		sessions:   make(map[string]*sessionEntry),
		restarts:   make(map[string]int),
		commands:   make(map[string]foundCommand),
	}
	seen := make(map[string]bool)
	for _, server := range slices.Concat(servers, defaultServers) {
		if !seen[server.Name] {
			seen[server.Name] = true
			manager.servers = append(manager.servers, cloneServer(server))
		}
	}
	return manager
}

func (m *Manager) WorkingDir() string {
	return m.workingDir
}

// FindServer never starts a server.
func (m *Manager) FindServer(filePath string) *Server {
	server, err := m.serverFor(filePath)
	if err != nil {
		return nil
	}
	server = cloneServer(server)
	return &server
}

// Each extension has one server: the first registered that claims it, installed or not.
func (m *Manager) configuredServer(filePath string) *Server {
	for i := range m.servers {
		if m.servers[i].LanguageIDForPath(filePath) != "" {
			return &m.servers[i]
		}
	}
	return nil
}

func (m *Manager) serverFor(filePath string) (Server, error) {
	kind := filepath.Ext(filePath)
	if kind == "" {
		kind = filepath.Base(filePath)
	}
	config := m.configuredServer(filePath)
	if config == nil {
		return Server{}, fmt.Errorf("no language server is configured for %s files", kind)
	}
	server := *config
	server.path = m.commandPath(server.Command)
	if server.path != "" {
		return server, nil
	}
	// An existing process can still serve requests after its executable moves.
	if session, ok := m.ActiveSession(filePath); ok {
		return session.server, nil
	}
	return Server{}, fmt.Errorf("language server %s for %s files is not installed: %q not found on PATH", server.Name, kind, server.Command)
}

func (m *Manager) commandPath(command string) string {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if found, ok := m.commands[command]; ok && time.Since(found.at) < commandCacheTTL {
		return found.path
	}
	path := m.lookPath(command)
	if path != "" {
		// A miss is not remembered, so a server works as soon as it is installed.
		m.commands[command] = foundCommand{path: path, at: time.Now()}
	}
	return path
}

func (m *Manager) GetSession(ctx context.Context, filePath string) (*Session, error) {
	server, err := m.serverFor(filePath)
	if err != nil {
		return nil, err
	}
	entry, err := m.getOrStartSession(server)
	if err != nil {
		return nil, err
	}
	select {
	case <-entry.done:
		return entry.session, entry.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) getOrStartSession(server Server) (*sessionEntry, error) {
	key := server.Name

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("LSP manager is closed")
	}

	previous := m.sessions[key]
	if previous != nil {
		if previous.session == nil || previous.session.IsAlive() {
			return previous, nil
		}
		if m.restarts[key] >= server.restarts() {
			return nil, fmt.Errorf("LSP server %s reached its restart limit (%d), not restarting", key, server.restarts())
		}
		m.restarts[key]++
	}
	entry := &sessionEntry{done: make(chan struct{})}
	m.sessions[key] = entry

	// Session startup is shared workspace state. Keep it alive when the request
	// that discovered the server is cancelled (for example, when a hover moves)
	// and let each caller stop waiting independently.
	go m.startSession(server, entry, previous)
	return entry, nil
}

func (m *Manager) startSession(server Server, entry, previous *sessionEntry) {
	ctx := m.lifecycle

	dir := m.workingDir
	if server.WorkspaceFolder != "" {
		if dir = server.WorkspaceFolder; !filepath.IsAbs(dir) {
			dir = filepath.Join(m.workingDir, dir)
		}
	}
	session, err := m.connect(ctx, dir, server)
	if err == nil && session == nil {
		err = fmt.Errorf("start %s: connector returned no session", server.Name)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && previous != nil {
		for _, doc := range previous.session.openedDocuments() {
			m.mu.Lock()
			closed := slices.Contains(entry.closed, doc.Path)
			m.mu.Unlock()
			if closed {
				continue
			}
			if doc.Saved && !doc.Editor {
				// Disk may have changed while the server was offline.
				_, err = session.OpenDocument(ctx, doc.Path)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
			} else {
				_, err = session.syncDocument(ctx, doc.Path, doc.Content, doc.Saved, doc.Editor)
			}
			if err != nil {
				err = fmt.Errorf("restore %s: %w", doc.Path, err)
				break
			}
		}
	}

	m.mu.Lock()
	// Finish closes received during replay before a caller can reopen a buffer.
	for len(entry.closed) > 0 && !m.closed {
		closed := entry.closed
		entry.closed = nil
		m.mu.Unlock()
		for _, path := range closed {
			if previous != nil {
				previous.session.forgetDocument(path)
			}
			if err == nil {
				err = session.CloseDocument(ctx, path)
			}
		}
		m.mu.Lock()
	}
	switch {
	case m.closed:
		err = fmt.Errorf("LSP manager is closed")
	case err == nil:
		entry.session = session
	case previous != nil:
		// Keep editor buffers for the next allowed restart attempt.
		m.sessions[server.Name] = previous
		err = fmt.Errorf("restart %s: %w", server.Name, err)
	default:
		delete(m.sessions, server.Name)
	}
	entry.err = err
	close(entry.done)
	m.mu.Unlock()
	if err != nil && session != nil {
		session.Close()
	}
}

// CloseDocument ends an editor document's lifetime, also while its server is
// stopped or restarting, so that a restart does not restore it.
func (m *Manager) CloseDocument(ctx context.Context, filePath string) error {
	server := m.configuredServer(filePath)
	if server == nil {
		return nil
	}
	m.mu.Lock()
	entry := m.sessions[server.Name]
	switch {
	case entry == nil:
	case entry.session == nil:
		entry.closed = append(entry.closed, filePath)
	case !entry.session.IsAlive():
		// Under m.mu, so a restart's snapshot of open documents excludes it.
		entry.session.forgetDocument(filePath)
	default:
		session := entry.session
		m.mu.Unlock()
		return session.CloseDocument(ctx, filePath)
	}
	m.mu.Unlock()
	return nil
}

// ActiveSession returns the file's session only if its server already runs.
func (m *Manager) ActiveSession(filePath string) (*Session, bool) {
	server := m.configuredServer(filePath)
	if server == nil {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if entry := m.sessions[server.Name]; entry != nil && entry.session != nil && entry.session.IsAlive() {
		return entry.session, true
	}
	return nil, false
}

func (m *Manager) liveSessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, entry := range m.sessions {
		if entry.session != nil && entry.session.IsAlive() {
			sessions = append(sessions, entry.session)
		}
	}
	return sessions
}

func (m *Manager) Running() bool {
	return len(m.liveSessions()) > 0
}

// FilesChanged passes disk changes to every live session.
func (m *Manager) FilesChanged(ctx context.Context, changes []FileChange) {
	for _, session := range m.liveSessions() {
		_ = session.FilesChanged(ctx, changes)
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, entry := range m.sessions {
		sessions = append(sessions, entry.session)
	}
	clear(m.sessions)
	m.mu.Unlock()
	m.cancel()

	var wait sync.WaitGroup
	for _, session := range sessions {
		if session != nil {
			wait.Go(session.Close)
		}
	}
	wait.Wait()
}
