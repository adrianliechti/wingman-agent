package terminal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/google/uuid"
)

type Manager struct {
	dir string

	mu       sync.Mutex
	order    []string
	sessions map[string]*Session // Nil after Close; no more sessions may be created.

	onExit func(id string)
	subs   map[chan Event]struct{}
}

// Event describes catalogue changes. Output has its own per-session stream.
type Event struct {
	Kind    string
	Session *Session
}

const MaxSessions = 128

func NewManager(dir string) *Manager {
	return &Manager{
		dir:      dir,
		sessions: map[string]*Session{},
		subs:     map[chan Event]struct{}{},
	}
}

func (m *Manager) SetExitHandler(fn func(id string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onExit = fn
}

func (m *Manager) Create(shell string, cols, rows int) (*Session, error) {
	return m.CreateShell(shell, "", "", cols, rows)
}

// CreateShell selects an installed shell and confines its working directory to
// this workspace, just like editor-owned commands.
func (m *Manager) CreateShell(shell, dir, title string, cols, rows int) (*Session, error) {
	resolved, ok := resolveShell(shell)
	if !ok {
		return nil, fmt.Errorf("unknown shell %q", shell)
	}
	dir, err := m.commandDir(dir)
	if err != nil {
		return nil, err
	}

	// Held across the whole creation so a shell that exits immediately cannot run
	// handleExit before the session is registered.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		return nil, errors.New("terminal manager is closed")
	}
	if len(m.sessions) >= MaxSessions {
		return nil, errors.New("terminal limit reached; dispose an existing terminal")
	}

	s, err := newCommandSession(uuid.NewString(), CommandSpec{Path: resolved, Dir: dir, Title: title}, cols, rows, m.handleExit)

	if err != nil {
		return nil, err
	}

	m.sessions[s.ID()] = s
	m.order = append(m.order, s.ID())
	m.publishLocked(Event{Kind: "created", Session: s})

	return s, nil
}

// CreateCommand starts a trusted editor-owned command directly in a PTY. The
// working directory remains constrained to the manager's workspace.
func (m *Manager) CreateCommand(spec CommandSpec, cols, rows int) (*Session, error) {
	dir, err := m.commandDir(spec.Dir)
	if err != nil {
		return nil, err
	}
	spec.Dir = dir

	// Held across creation so a process that exits immediately cannot run
	// handleExit before the session is registered.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		return nil, errors.New("terminal manager is closed")
	}
	if len(m.sessions) >= MaxSessions {
		return nil, errors.New("terminal limit reached; dispose an existing terminal")
	}

	session, err := newCommandSession(uuid.NewString(), spec, cols, rows, m.handleExit)
	if err != nil {
		return nil, err
	}
	session.command = true
	m.sessions[session.ID()] = session
	m.order = append(m.order, session.ID())
	m.publishLocked(Event{Kind: "created", Session: session})
	return session, nil
}

func (m *Manager) commandDir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = m.dir
	} else if !filepath.IsAbs(value) {
		value = filepath.Join(m.dir, value)
	}
	dir, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve terminal working directory: %w", err)
	}
	root, err := filepath.Abs(m.dir)
	if err != nil {
		return "", fmt.Errorf("resolve terminal workspace: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("terminal working directory must stay inside the workspace")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("terminal working directory %q is not a directory", value)
	}
	resolvedRoot, err := pathutil.Resolve(root)
	if err != nil {
		return "", fmt.Errorf("resolve terminal workspace symlinks: %w", err)
	}
	resolvedDir, err := pathutil.Resolve(dir)
	if err != nil {
		return "", fmt.Errorf("resolve terminal working directory symlinks: %w", err)
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedDir)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return "", errors.New("terminal working directory must stay inside the workspace after resolving symlinks")
	}
	return dir, nil
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked()
}

func (m *Manager) listLocked() []*Session {
	out := make([]*Session, 0, len(m.order))
	for _, id := range m.order {
		if s, ok := m.sessions[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) Remove(id string) bool {
	s := m.take(id)
	if s == nil {
		return false
	}
	_ = s.Close()
	return true
}

func (m *Manager) Close() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = nil
	m.order = nil
	m.onExit = nil
	for ch := range m.subs {
		close(ch)
		delete(m.subs, ch)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.Close()
	}
}

func (m *Manager) take(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil
	}
	delete(m.sessions, id)
	m.publishLocked(Event{Kind: "removed", Session: s})
	if i := slices.Index(m.order, id); i >= 0 {
		m.order = slices.Delete(m.order, i, i+1)
	}
	return s
}

func (m *Manager) handleExit(s *Session) {
	m.mu.Lock()
	fn := m.onExit
	if m.sessions[s.ID()] == s {
		m.publishLocked(Event{Kind: "exited", Session: s})
	}
	m.mu.Unlock()

	if fn != nil {
		fn(s.ID())
	}
}

// Subscribe atomically captures the catalogue and starts observing changes.
// A slow observer is disconnected and must subscribe again for a fresh view.
func (m *Manager) Subscribe() ([]*Session, <-chan Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan Event, 64)
	if m.sessions == nil {
		close(ch)
	} else {
		m.subs[ch] = struct{}{}
	}
	return m.listLocked(), ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.subs[ch]; ok {
			delete(m.subs, ch)
			close(ch)
		}
	}
}

func (m *Manager) publishLocked(event Event) {
	for ch := range m.subs {
		select {
		case ch <- event:
		default:
			delete(m.subs, ch)
			close(ch)
		}
	}
}
