package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/adrianliechti/wingman-agent/pkg/watch"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Project owns one workspace, its agents and protocol channels.
type Project struct {
	*harness.Config

	workspace *code.Workspace
	telemetry *telemetry.Telemetry

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce  sync.Once
	background sync.WaitGroup
	operations sync.WaitGroup // backend I/O admitted under backendsMu

	backendsMu sync.Mutex
	backends   map[string]*backendEntry
	scope      WorkspaceScope

	fileWriteMu sync.Mutex

	terminals *terminal.Manager

	fileMonitor     *watch.Monitor
	skillFiles      *watch.Monitor
	prevGit         bool
	prevFingerprint uint64

	host             *Host
	dir              string    // the project's identity: its directory with links resolved
	namespace        string    // prefix of the channels this project names itself
	used             time.Time // when a client last reached the project; protected by host.mu
	terminalOnce     sync.Once
	terminalChannels map[string]*terminalChannel
	watches          map[string]*resourceWatch
	watchOnce        sync.Once
	changesMu        sync.Mutex
	changesOnce      sync.Once
	changesets       map[string]*changeset
	storageRoot      string  // the repository that keeps its builtin journals, while its folder tells
	window           *Server // web adapter for a secondary project, published under host.mu
	windowRequests   int     // open scoped HTTP requests and WebSockets, protected by host.mu
	temporary        func()  // removes a temporary chat's folder as the project closes; nil for any other project
}

// newProject starts a project's resources under the host's lifetime.
func (h *Host) newProject(workDir string) (_ *Project, err error) {
	cfg, err := agent.DefaultConfig()
	if err != nil {
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(h.ctx)
	s := &Project{
		telemetry: cfg.Telemetry,
		ctx:       serverCtx,
		cancel:    cancel,
		backends:  make(map[string]*backendEntry),
	}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	if dir, err := filepath.Abs(workDir); err == nil && isTemporary(canonicalPath(dir)) {
		workDir = canonicalPath(dir)
		if s.temporary, err = claimTemporary(workDir); err != nil {
			return nil, err
		}
	}
	ws, err := code.NewWorkspace(workDir)
	if err != nil {
		return nil, err
	}
	s.workspace = ws
	s.scope = workspaceScope(ws.RootPath)
	h.attach(s)

	s.terminals = terminal.NewManager(ws.RootPath)

	cfg.IsolateSessions = true
	wa := agent.New(ws, cfg, nil)
	rt := s.bindBackend(code.BuiltinAgentName, wa)
	s.backends[code.BuiltinAgentName] = &backendEntry{runtime: rt}
	s.Config = cfg.Clone()
	s.RoleModel = wa.RoleModel
	s.Model = func() string {
		option, _ := wa.RoleModel("")
		return option.ID
	}
	ws.WarmUp()

	s.prevGit = ws.IsGitRepo()
	s.fileMonitor = watch.New(watch.Options{Active: s.hasClients}, s.checkWorkspace)
	s.background.Go(func() {
		s.fileMonitor.Run(serverCtx)
	})
	s.skillFiles = watch.New(watch.Options{
		Fallback: 2 * time.Second,
		Active:   s.hasClients,
	}, s.refreshSkills)
	s.background.Go(func() {
		s.skillFiles.Run(serverCtx)
	})

	s.background.Go(func() {
		if err := ws.InitMCP(serverCtx); err != nil && serverCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "MCP init warning: %v\n", err)
		}
	})

	s.background.Go(func() {
		if err := rt.refreshModels(); err != nil && serverCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "Model discovery warning: %v\n", err)
		}
	})

	return s, nil
}

// Operations may nest (commands refresh metadata), but admission closes before
// shutdown waits. Backend resources outlive every admitted operation.
func (p *Project) beginOperation() bool {
	p.backendsMu.Lock()
	defer p.backendsMu.Unlock()
	if p.ctx.Err() != nil {
		return false
	}
	p.operations.Add(1)
	return true
}

func (s *Project) close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.host != nil {
			s.detach()
		}

		// Taking the admission lock after cancellation joins any last admission.
		s.backendsMu.Lock()
		runtimes := make([]*backendRuntime, 0, len(s.backends))
		for _, entry := range s.backends {
			rt := entry.runtime
			if rt == nil {
				continue
			}
			runtimes = append(runtimes, rt)
		}
		s.backendsMu.Unlock()
		for _, rt := range runtimes {
			rt.turns.SetHandler(nil)
			rt.turns.Close()
		}
		s.operations.Wait()
		for _, rt := range runtimes {
			_ = rt.agent.Close()
		}
		s.background.Wait()
		if s.host != nil {
			s.host.saveAliases()
			if s.window != nil {
				s.window.preview.Close()
			}
		}
		if s.workspace != nil {
			s.workspace.Close()
		}
		if s.terminals != nil {
			s.terminals.Close()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.telemetry.Shutdown(shutdownCtx)
		cancel()
		if s.temporary != nil {
			if s.host != nil {
				s.host.forgetTemporary(s.dir)
			}
			s.temporary()
		}
	})
}

// attach makes a project and its saved sessions reachable through the host.
func (h *Host) attach(p *Project) *Project {
	p.host = h
	p.terminalChannels = map[string]*terminalChannel{}
	p.namespace = "wingman/" + p.scope.WorkspaceID[:16]
	p.used = time.Now()
	if p.workspace != nil {
		p.dir = canonicalPath(p.workspace.RootPath)
	}
	p.storageRoot = sessionAlias{Backend: code.BuiltinAgentName, Project: p.dir}.rooted().StorageRoot
	var saved []sessionAlias
	if dir, err := p.aliasDirectory(); err == nil {
		saved = readAliases(dir)
	}
	for i := range saved {
		saved[i].Project = p.dir
		saved[i] = saved[i].rooted()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, alias := range saved {
		h.rememberAliasLocked(alias)
	}
	return p
}

// show makes a project the one the host's web UI shows.
func (h *Host) show(p *Project) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.web = p
	h.projects[p.dir] = p
}

// Only web windows follow a project's files; each finds its own by the workspace.
func (p *Project) workspaceChanged(f Frame) {
	if p == nil || p.host == nil {
		return
	}
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if p.host.web == p || p.window != nil {
		p.host.notifyRoot("x-wingman/workspaceChanged", map[string]any{"channel": types.RootResourceURI, "event": f, "workspace": p.scope.WorkspaceID})
	}
}

// stop cancels the project's work before its resources are released.
func (p *Project) stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

// availableLocked rejects late attachments from work that was already in flight.
// Caller holds host.mu.
func (p *Project) availableLocked() bool {
	return p.host.ctx.Err() == nil && p.ctx.Err() == nil
}

// detachLocked removes live channels while retaining aliases for reopening.
// Caller holds host.mu.
func (p *Project) detachLocked() {
	p.stop()
	p.unlistLocked()
}

// unlistLocked takes the project and its sessions out of the host, so that no
// client reaches them, while its work keeps its context. Caller holds host.mu.
func (p *Project) unlistLocked() {
	if p.host.projects[p.dir] == p {
		delete(p.host.projects, p.dir)
	}
	for resource, a := range p.host.sessions {
		if a.project != p {
			continue
		}
		for _, ch := range a.chats {
			delete(p.host.chats, ch.chat.Resource)
		}
		delete(p.host.sessions, resource)
	}
}

// detach ends the project.
func (p *Project) detach() {
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	p.detachLocked()
}

// info is the spec's description of the project, which its sessions carry.
// It names the project by its directory with the links resolved, as saved
// sessions do, so that a project started through a link is one project.
func (p *Project) info() *types.ProjectInfo {
	if p.workspace == nil {
		return nil
	}
	return projectInfo(p.dir)
}

func (p *Project) directoryURI() string { return p.fileURI(".") }

func (p *Project) startedBackend(provider string) *backendRuntime {
	p.backendsMu.Lock()
	defer p.backendsMu.Unlock()
	if entry := p.backends[provider]; entry != nil {
		return entry.runtime
	}
	return nil
}
