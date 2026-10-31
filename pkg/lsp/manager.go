package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/tooling"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
)

const maxRestarts = 3

type Manager struct {
	workingDir string
	lifecycle  context.Context
	cancel     context.CancelFunc
	connect    sessionConnector
	sessions   map[string]*sessionEntry
	restarts   map[string]int
	closed     bool
	mu         sync.Mutex

	detectMu              sync.Mutex
	roots                 []projectRoot
	detectedAt            time.Time
	initializationOptions map[string][]byte
	managedTools          tooling.ManagedTools
	registry              *Registry
}

// ServerActivity describes one live language-server session without starting
// a new one. It is intended for lightweight workspace status surfaces.
type ServerActivity struct {
	Server     string         `json:"server"`
	Label      string         `json:"label"`
	ProjectDir string         `json:"project_dir"`
	Analyzing  bool           `json:"analyzing"`
	Operations []WorkProgress `json:"operations"`
}

type ManagerOption func(*Manager)

// WithServerInitializationOptions adds opaque initialization options to every
// detected server with the given name. The options are marshalled once and
// copied into each server descriptor before a session starts.
func WithServerInitializationOptions(name string, value any) ManagerOption {
	return func(manager *Manager) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return
		}
		if manager.initializationOptions == nil {
			manager.initializationOptions = make(map[string][]byte)
		}
		manager.initializationOptions[serverOptionsKey(name)] = encoded
	}
}

// WithManagedTools requires Wingman's managed copy for supported language tools.
func WithManagedTools(tools tooling.ManagedTools) ManagerOption {
	return func(manager *Manager) {
		manager.managedTools = tools
	}
}

// sessionEntry is shared by warmup and on-demand callers. The result is
// published by closing done; failed entries are removed so later calls retry.
type sessionEntry struct {
	done    chan struct{}
	session *Session
	err     error
	closed  []string // documents closed while the session starts
}

type sessionConnector func(context.Context, string, Server) (*Session, error)

const detectionCacheTTL = 30 * time.Second

func NewManager(workingDir string, options ...ManagerOption) *Manager {
	lifecycle, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		workingDir: workingDir,
		lifecycle:  lifecycle,
		cancel:     cancel,
		connect:    connect,
		sessions:   make(map[string]*sessionEntry),
		restarts:   make(map[string]int),
	}
	for _, option := range options {
		if option != nil {
			option(manager)
		}
	}
	return manager
}

func (m *Manager) WorkingDir() string {
	return m.workingDir
}

func (m *Manager) detect() []projectRoot {
	m.detectMu.Lock()
	defer m.detectMu.Unlock()
	if m.detectedAt.IsZero() || time.Since(m.detectedAt) >= detectionCacheTTL {
		m.roots = detectAll(m.workingDir, m.managedTools, m.registry)
		for index := range m.roots {
			if options := m.initializationOptions[serverOptionsKey(m.roots[index].Server.Name)]; len(options) > 0 {
				m.roots[index].Server.InitializationOptions = slices.Clone(options)
			}
		}
		m.detectedAt = time.Now()
	}
	return cloneProjectRoots(m.roots)
}

// Detection results are cached, but every public lookup returns an isolated
// descriptor. Server contains slices, so a shallow copy would let a caller
// mutate the cache (and, for catalog-backed fields, the global catalog) by
// changing Args, Languages, or InitializationOptions in a returned value.
func cloneProjectRoots(values []projectRoot) []projectRoot {
	cloned := make([]projectRoot, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].Server.Args = slices.Clone(value.Server.Args)
		cloned[index].Server.Languages = slices.Clone(value.Server.Languages)
		cloned[index].Server.ExtensionToLanguage = maps.Clone(value.Server.ExtensionToLanguage)
		cloned[index].Server.InitializationOptions = slices.Clone(value.Server.InitializationOptions)
	}
	return cloned
}

// InvalidateDetection makes newly installed or updated servers visible on the
// next lookup without waiting for the detection cache TTL. Crash counters are
// reset because an updated tool deserves a fresh restart budget.
func (m *Manager) InvalidateDetection() {
	m.detectMu.Lock()
	m.detectedAt = time.Time{}
	m.detectMu.Unlock()
	m.mu.Lock()
	clear(m.restarts)
	m.mu.Unlock()
}

// SetServerInitializationOptions updates future sessions for a server. An
// active session for that server is closed because LSP initialization options
// cannot be changed after startup.
func (m *Manager) SetServerInitializationOptions(name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	optionsKey := serverOptionsKey(name)
	m.detectMu.Lock()
	if bytes.Equal(m.initializationOptions[optionsKey], encoded) {
		m.detectMu.Unlock()
		return nil
	}
	if m.initializationOptions == nil {
		m.initializationOptions = make(map[string][]byte)
	}
	m.initializationOptions[optionsKey] = encoded
	m.detectedAt = time.Time{}
	m.detectMu.Unlock()

	var stale []*Session
	m.mu.Lock()
	for key, entry := range m.sessions {
		if entry.session != nil && strings.EqualFold(entry.session.server.Name, name) {
			stale = append(stale, entry.session)
			delete(m.sessions, key)
		}
	}
	m.mu.Unlock()
	closeSessions(stale)
	return nil
}

func serverOptionsKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// serverInitializationOptionsCurrentLocked reports whether a descriptor still
// carries the manager's current options. Callers hold detectMu so an options
// update cannot race the decision to publish a newly connected session.
func (m *Manager) serverInitializationOptionsCurrentLocked(server Server) bool {
	options, configured := m.initializationOptions[serverOptionsKey(server.Name)]
	return !configured || bytes.Equal(server.InitializationOptions, options)
}

func (m *Manager) FindServer(filePath string) *Server {
	project := m.findProject(filePath)
	if project == nil {
		return nil
	}
	server := project.Server
	return &server
}

func (m *Manager) findProject(filePath string) *projectRoot {
	return findProject(m.detect(), filePath)
}

func findProject(projects []projectRoot, filePath string) *projectRoot {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filePath), "."))
	if ext == "" {
		return nil
	}

	dir := filepath.Dir(filePath)
	var best *projectRoot
	bestLen := -1

	for _, root := range projects {
		if !isSubPath(root.Dir, dir) {
			continue
		}
		if len(root.Dir) <= bestLen {
			continue
		}
		if !slices.Contains(root.Server.Languages, ext) {
			continue
		}
		candidate := root
		best = &candidate
		bestLen = len(root.Dir)
	}

	return best
}

func (m *Manager) DetectServers() []Server {
	var servers []Server
	seen := make(map[string]bool)

	for _, root := range m.detect() {
		key := serverKey(root.Server)
		if seen[key] {
			continue
		}
		seen[key] = true
		servers = append(servers, root.Server)
	}

	return servers
}

func (m *Manager) Projects() []Project { return m.detect() }

func (m *Manager) ProjectSession(ctx context.Context, project Project) (*Session, error) {
	return m.getSession(ctx, project)
}

func (m *Manager) WarmUpFile(filePath string) {
	if project := m.findProject(filePath); project != nil {
		_, _ = m.getOrStartSession(*project)
	}
}

func projectKey(project projectRoot) string {
	return filepath.Clean(project.Dir) + "\x00" + serverKey(project.Server)
}

func serverKey(server Server) string {
	return server.Command + "\x00" + strings.Join(server.Args, "\x00") + "\x00" + string(server.InitializationOptions)
}

func (m *Manager) GetSession(ctx context.Context, filePath string) (*Session, error) {
	project := m.findProject(filePath)
	if project == nil {
		return nil, fmt.Errorf("no LSP server found for file: %s", filePath)
	}

	return m.getSession(ctx, *project)
}

func (m *Manager) getSession(ctx context.Context, project projectRoot) (*Session, error) {
	entry, err := m.getOrStartSession(project)
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

func (m *Manager) getOrStartSession(project projectRoot) (*sessionEntry, error) {
	server := project.Server
	key := projectKey(project)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("LSP manager is closed")
	}

	var openedDocuments []openDocument
	restarting := false
	if previous := m.sessions[key]; previous != nil {
		if previous.session == nil || previous.session.IsAlive() {
			return previous, nil
		}
		openedDocuments = previous.session.openedDocuments()
		delete(m.sessions, key)
		m.restarts[key]++
		restarting = true
	}
	if m.restarts[key] > maxRestarts {
		return nil, fmt.Errorf("LSP server %s crashed %d times, not restarting", server.Name, maxRestarts)
	}
	entry := &sessionEntry{done: make(chan struct{})}
	m.sessions[key] = entry

	// Session startup is shared workspace state. Keep it alive when the request
	// that discovered the server is cancelled (for example, when a hover moves)
	// and let each caller stop waiting independently.
	go m.startSession(project, key, entry, openedDocuments, restarting)
	return entry, nil
}

func (m *Manager) startSession(project projectRoot, key string, entry *sessionEntry, openedDocuments []openDocument, restarting bool) {
	server := project.Server
	parent := m.lifecycle
	if parent == nil {
		parent = context.Background()
	}
	connectCtx, cancelConnect := context.WithCancel(parent)
	defer cancelConnect()

	connector := m.connect
	if connector == nil {
		connector = connect
	}
	session, err := connector(connectCtx, project.Dir, server)
	if err == nil && session == nil {
		err = fmt.Errorf("start %s: connector returned no session", server.Name)
	}
	if err == nil {
		err = connectCtx.Err()
	}
	if err == nil {
		for _, document := range openedDocuments {
			m.mu.Lock()
			closed := slices.Contains(entry.closed, document.Path)
			m.mu.Unlock()
			if document.Path == "" || closed {
				continue
			}
			var syncErr error
			if document.Editor {
				_, syncErr = session.SyncEditorDocument(connectCtx, document.Path, document.Content, document.Saved)
			} else if document.Saved {
				_, syncErr = session.SaveDocument(connectCtx, document.Path, document.Content)
			} else {
				_, syncErr = session.SyncDocument(connectCtx, document.Path, document.Content)
			}
			if syncErr != nil {
				err = fmt.Errorf("restore open document %s: %w", document.Path, syncErr)
				break
			}
		}
	}
	if err != nil && restarting {
		err = fmt.Errorf("restart %s: %w", server.Name, err)
	}

	m.detectMu.Lock()
	m.mu.Lock()
	if m.closed {
		err = fmt.Errorf("LSP manager is closed")
	} else if err == nil && !m.serverInitializationOptionsCurrentLocked(server) {
		err = fmt.Errorf("LSP server %s initialization options changed during startup", server.Name)
	}
	if err == nil {
		entry.session = session
	} else {
		delete(m.sessions, key)
	}
	entry.err = err
	closed := entry.closed
	entry.closed = nil
	close(entry.done)
	m.mu.Unlock()
	m.detectMu.Unlock()

	if err != nil && session != nil {
		session.Close()
	}
	if err == nil {
		for _, path := range closed {
			_ = session.CloseDocument(connectCtx, path)
		}
	}
}

// CloseDocument ends an editor document's lifetime, also while its server is
// stopped or restarting, so that a restart does not restore it.
func (m *Manager) CloseDocument(ctx context.Context, filePath string) error {
	project := m.findProject(filePath)
	if project == nil {
		return nil
	}
	m.mu.Lock()
	entry := m.sessions[projectKey(*project)]
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

func (m *Manager) ActiveSession(filePath string) (*Session, bool) {
	project := m.findProject(filePath)
	if project == nil {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if entry := m.sessions[projectKey(*project)]; entry != nil && entry.session != nil && entry.session.IsAlive() {
		return entry.session, true
	}
	return nil, false
}

// FilesChanged passes disk changes to every live session.
func (m *Manager) FilesChanged(ctx context.Context, changes []FileChange) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, entry := range m.sessions {
		if entry.session != nil && entry.session.IsAlive() {
			sessions = append(sessions, entry.session)
		}
	}
	m.mu.Unlock()
	for _, session := range sessions {
		_ = session.FilesChanged(ctx, changes)
	}
}

func (m *Manager) Activities() []ServerActivity {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, entry := range m.sessions {
		if entry.session != nil && entry.session.IsAlive() {
			sessions = append(sessions, entry.session)
		}
	}
	m.mu.Unlock()

	result := make([]ServerActivity, 0, len(sessions))
	for _, session := range sessions {
		projectDir, _ := fileuri.Path(session.rootURI)
		operations := session.Progress()
		label := session.server.Label
		if label == "" {
			label = session.server.Name
		}
		result = append(result, ServerActivity{
			Server:     session.server.Name,
			Label:      label,
			ProjectDir: projectDir,
			Analyzing:  len(operations) > 0,
			Operations: operations,
		})
	}
	slices.SortFunc(result, func(a, b ServerActivity) int {
		if order := strings.Compare(a.ProjectDir, b.ProjectDir); order != 0 {
			return order
		}
		return strings.Compare(a.Server, b.Server)
	})
	return result
}

func (m *Manager) WarmUpServers() {
	go func() {
		if m.lifecycle != nil && m.lifecycle.Err() != nil {
			return
		}
		for _, project := range m.detect() {
			if m.lifecycle != nil && m.lifecycle.Err() != nil {
				return
			}
			_, _ = m.getOrStartSession(project)
		}
	}()
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
	if m.cancel != nil {
		m.cancel()
	}

	closeSessions(sessions)
}

func closeSessions(sessions []*Session) {
	var wait sync.WaitGroup
	for _, session := range sessions {
		if session == nil {
			continue
		}
		wait.Go(session.Close)
	}
	wait.Wait()
}
