package server

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

type managedToolsStatus struct {
	State       string                 `json:"state"`
	Tool        string                 `json:"tool,omitempty"`
	Label       string                 `json:"label,omitempty"`
	Phase       devtools.ProgressPhase `json:"phase,omitempty"`
	Current     int                    `json:"current,omitempty"`
	Total       int                    `json:"total,omitempty"`
	Error       string                 `json:"error,omitempty"`
	Unavailable []string               `json:"unavailable,omitempty"`
}

func (s *Project) setManagedToolsStatus(status managedToolsStatus) {
	s.managedTools.Store(&status)
	s.workspaceChanged(Frame{Type: EvtWorkspaceChanged})
}

func (s *Project) managedToolsStatus() managedToolsStatus {
	if status := s.managedTools.Load(); status != nil {
		return *status
	}
	return managedToolsStatus{}
}

func (s *Project) debugAvailable(ctx context.Context) bool {
	available := false
	_ = s.workspace.WithDAPManager(func(manager *dap.Manager) error {
		if session := manager.ActiveSession(); session != nil && session.Status().State != dap.StateTerminated {
			available = true
			return nil
		}
		adapters, err := manager.Adapters(ctx)
		available = err == nil && len(adapters) > 0
		return nil
	})
	return available
}

// refreshDiagnostics notifies the optional editor adapter outside the host lock.
func (s *Project) refreshDiagnostics() {
	if s.host == nil {
		return
	}
	s.host.mu.Lock()
	refresh := s.onDiagnostics
	s.host.mu.Unlock()
	if refresh != nil {
		refresh()
	}
}

func (s *Project) hasClients() bool {
	return s != nil && s.host != nil && s.host.connected()
}

func (s *Project) flushFiles() {
	if s.fileMonitor != nil {
		s.fileMonitor.Flush()
	}
	s.workspaceChanged(Frame{Type: EvtFilesChanged})
}

func (s *Project) checkWorkspace() {
	ws := s.workspace

	gitNow := ws.IsGitRepo()
	if gitNow != s.prevGit {
		ws.SyncProjectMode()
		s.workspaceChanged(Frame{Type: EvtWorkspaceChanged})
		if ws.HasLSP() {
			s.refreshDiagnostics()
		}
		s.prevGit = gitNow
	}

	lspNow := ws.HasLSP()
	if lspNow != s.prevLSP {
		s.prevLSP = lspNow
		s.workspaceChanged(Frame{Type: EvtWorkspaceChanged})
		if lspNow {
			s.refreshDiagnostics()
		}
	}

	debugNow := s.debugAvailable(s.ctx)
	if debugNow != s.prevDebug {
		s.prevDebug = debugNow
		s.workspaceChanged(Frame{Type: EvtWorkspaceChanged})
	}

	if ws.HasLSP() {
		s.scanWorkspaceFiles()
	}
	if !ws.HasChanges() {
		s.workspaceChanged(Frame{Type: EvtFilesChanged})
		if ws.HasLSP() {
			s.refreshDiagnostics()
		}
		return
	}
	fp := ws.ChangesFingerprint(s.ctx)
	if fp != s.prevFingerprint {
		s.prevFingerprint = fp
		s.workspaceChanged(Frame{Type: EvtFilesChanged})
		if ws.HasLSP() {
			s.refreshDiagnostics()
		}
	}
}

// Dependency, build and hidden directories are too large or irrelevant to watch.
func isExcludedDir(name string) bool { return strings.HasPrefix(name, ".") || excludedDirs[name] }

// scanWorkspaceFiles passes disk changes to language servers. A slow walk
// or language server must not stall the workspace poller.
func (s *Project) scanWorkspaceFiles() {
	if !s.fileScanning.CompareAndSwap(false, true) {
		return
	}
	s.background.Go(func() {
		defer s.fileScanning.Store(false)
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		s.syncWorkspaceFiles(ctx)
	})
}

func (s *Project) syncWorkspaceFiles(ctx context.Context) {
	changes, err := s.workspaceFiles.Changes(ctx)
	if err != nil || len(changes) == 0 {
		return
	}
	events := make([]lsp.FileChange, 0, len(changes))
	for _, change := range changes {
		events = append(events, lsp.FileChange{Path: filepath.Join(s.workspace.RootPath, filepath.FromSlash(change.Path)), Type: lsp.FileChangeType(change.Type)})
	}
	s.workspace.LSPFilesChanged(ctx, events)
}

func (s *Project) refreshSkills() {
	if s.workspace.RefreshSkills() {
		s.workspaceChanged(Frame{Type: EvtSkillsChanged})
	}
}
