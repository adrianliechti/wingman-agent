package server

import (
	"slices"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpProject is one project's share of a host: agents, sessions, terminals, watches and changesets.
type ahpProject struct {
	*ahpHost
	server       *Server
	dir          string     // the project's identity: its directory with links resolved
	namespace    string     // prefix of the channels this project names itself
	aliasMu      sync.Mutex // orders the saved aliases' writes, which happen outside the host's lock
	used         time.Time  // when a client last reached the project; protected by host.mu
	done         chan struct{}
	stopOnce     sync.Once
	terminalOnce sync.Once
	terminals    map[string]*ahpTerminal
	watches      map[string]*ahpResourceWatch
	watchOnce    sync.Once
	changesMu    sync.Mutex
	changesOnce  sync.Once
	changesets   map[string]*ahpChangeset
}

// attach makes a project and its saved sessions reachable through the host.
func (h *ahpHost) attach(s *Server) *ahpProject {
	p := &ahpProject{ahpHost: h, server: s, terminals: map[string]*ahpTerminal{}, namespace: "wingman", used: time.Now(), done: make(chan struct{})}
	if s.workspace != nil {
		p.dir = canonicalPath(s.workspace.RootPath)
	}
	if h.open == nil {
		h.pinned = p
	} else {
		// Several projects share the host's channel names.
		p.namespace += "/" + s.scope.WorkspaceID[:16]
	}
	var saved []ahpAlias
	if dir, err := p.aliasDirectory(); err == nil {
		saved = readAliases(dir)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, alias := range saved {
		alias.Project = p.dir
		h.rememberAliasLocked(alias)
	}
	return p
}

// ahpProject is the server's project on its host; a workspace server has its own host.
func (s *Server) ahpProject() *ahpProject {
	s.ahpOnce.Do(func() { s.ahp = newAHPHost().attach(s) })
	return s.ahp
}

// Only the clients of a workspace server follow one project's files.
func (p *ahpProject) workspaceChanged(f Frame) {
	if p.pinned == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifyRoot("x-wingman/workspaceChanged", map[string]any{"channel": t.RootResourceURI, "event": f})
}

// stop ends the project's own goroutines.
func (p *ahpProject) stop() {
	p.stopOnce.Do(func() { close(p.done) })
}

// availableLocked rejects late attachments from work that was already in flight.
// Caller holds host.mu.
func (p *ahpProject) availableLocked() bool {
	if p.closed {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// detachLocked removes live channels while retaining aliases for reopening.
// Caller holds host.mu.
func (p *ahpProject) detachLocked() {
	p.stop()
	if p.projects[p.dir] == p {
		delete(p.projects, p.dir)
	}
	for resource, a := range p.sessions {
		if a.project != p {
			continue
		}
		for _, ch := range a.chats {
			delete(p.chats, ch.chat.Resource)
		}
		delete(p.sessions, resource)
	}
}

// detach ends the project. The host of a workspace server ends with it.
func (p *ahpProject) detach() {
	p.mu.Lock()
	p.detachLocked()
	p.mu.Unlock()
	if p.pinned != nil {
		p.ahpHost.close()
	}
}

// info is the spec's description of the project, which its sessions carry.
func (p *ahpProject) info() *t.ProjectInfo {
	if p.server.workspace == nil {
		return nil
	}
	return projectInfo(p.server.workspace.RootPath)
}

func (p *ahpProject) directoryURI() string {
	root := "/"
	if p.server.workspace != nil {
		root = p.server.workspace.RootPath
	}
	return fileuri.FromPath(root)
}

func (p *ahpProject) startedBackend(provider string) *backendRuntime {
	p.server.backendsMu.Lock()
	defer p.server.backendsMu.Unlock()
	if entry := p.server.backends[provider]; entry != nil {
		return entry.runtime
	}
	return nil
}

// backend resolves an agent provider, reporting an unknown one as AHP's
// ProviderNotFound rather than a startup failure.
func (p *ahpProject) backend(provider string) (*backendRuntime, error) {
	b, err := p.server.backend(provider)
	if err == nil {
		return b, nil
	}
	known := provider == code.BuiltinAgentName
	if p.server.workspace != nil && !known {
		external, _ := agents.Available()
		known = slices.ContainsFunc(external, func(a agents.Registration) bool { return a.ID == provider })
	}
	if !known {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeProviderNotFound), Message: "unknown agent provider: " + provider}
	}
	return nil, err
}
