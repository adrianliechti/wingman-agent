package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/elicitation"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fetch"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/websearch"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

type sessionLock struct {
	token chan struct{}
	refs  int // Holder and waiters; protected by Agent.mu.
}

// Recovery, saving and deletion serialize per session. The channel allows a
// caller to cancel its wait; the map entry lives only while callers use it.
func (a *Agent) lockSession(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	lock := a.sessionLocks[id]
	if lock == nil {
		lock = &sessionLock{token: make(chan struct{}, 1)}
		if a.sessionLocks == nil {
			a.sessionLocks = make(map[string]*sessionLock)
		}
		a.sessionLocks[id] = lock
	}
	lock.refs++
	a.mu.Unlock()
	release := func() {
		a.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(a.sessionLocks, id)
		}
		a.mu.Unlock()
	}
	select {
	case lock.token <- struct{}{}:
		unlock := func() {
			<-lock.token
			release()
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// sessionResources owns the tools' dependencies. Its zero value is safe to
// close, so incomplete construction follows the same cleanup path.
type sessionResources struct {
	hooks       *external.Async
	tasks       *task.Registry
	execManager *shell.ExecManager
	tempDir     string
}

var _ io.Closer = (*sessionResources)(nil)

func (r *sessionResources) Close() error {
	r.hooks.Close()
	// Children share execution sessions, so cancel them before closing exec.
	r.tasks.Close()
	if r.execManager != nil {
		r.execManager.Close()
	}
	if r.tempDir != "" {
		if err := os.RemoveAll(r.tempDir); err != nil {
			return fmt.Errorf("remove session temporary files: %w", err)
		}
	}
	return nil
}

func (a *Agent) buildSession(id string) (*sessionState, error) {
	journal, err := session.OpenJournal(a.sessionsDir, id)
	if err != nil {
		return nil, err
	}
	s := &sessionState{
		id:      id,
		parent:  a,
		harness: &harness.Agent{Config: a.cfg.Clone(), Recorder: journal},
	}
	s.harness.Resources = &s.resources
	s.configureHarness()
	// Restoring subagent tools derives child configs immediately, so hooks
	// must be complete before constructing the catalog.
	s.configureHooks()
	if err := s.configureTools(); err != nil {
		return nil, errors.Join(err, s.resources.Close())
	}
	return s, nil
}

// configureHarness binds the session's live host policy before tools capture it.
func (s *sessionState) configureHarness() {
	a, sessionCfg := s.parent, s.harness.Config
	s.harness.CacheKey = s.id
	sessionCfg.Tools = s.catalogTools
	sessionCfg.PinTools = true
	sessionCfg.FilterTools = s.filterTools
	s.harness.PrepareTurn = s.prepareTurn
	sessionCfg.Instructions = s.instructions
	sessionCfg.ContextInstructions = s.contextInstructions
	sessionCfg.PermissionMode = s.permissionMode
	if sessionCfg.RequireFinish == nil {
		sessionCfg.RequireFinish = requiresFinish
	}
	sessionCfg.Model = func() string {
		option, _ := a.roleModel(s, "")
		return option.ID
	}
	sessionCfg.Effort = func() string {
		return a.effortFor(s)
	}
	sessionCfg.RoleModel = func(role string) (harness.ModelOption, bool) {
		return a.roleModel(s, role)
	}
}

// configureTools fills the session's resources before publishing its tools.
// buildSession closes the resource owner if setup fails.
func (s *sessionState) configureTools() error {
	a, sessionCfg := s.parent, s.harness.Config
	ws, elicit := a.workspace, a.prompts
	allowedReadRoots, allowedWriteRoots := sessionFileRoots(ws)
	if !a.cfg.DisableShell && !a.cfg.DisableSandbox {
		if err := shell.CheckSandbox(ws.RootPath); err != nil {
			return fmt.Errorf("sandbox unavailable (WINGMAN_SANDBOX=off explicitly disables isolation): %w", err)
		}
	}

	artifactDir, err := session.ArtifactDir(a.sessionsDir, s.id)
	if err != nil {
		return err
	}
	privateTemp, err := os.MkdirTemp("", "wingman-session-")
	if err != nil {
		return fmt.Errorf("create private session temp: %w", err)
	}
	// Use the sandbox's spelling everywhere (macOS /var is /private/var; Windows junctions/short names).
	privateTemp = sandboxpolicy.Canonical(privateTemp)
	s.resources.tempDir = privateTemp
	s.resources.tasks, err = task.NewFileRegistry(filepath.Join(artifactDir, "tasks.json"))
	if err != nil {
		return fmt.Errorf("restore background tasks: %w", err)
	}
	s.resources.execManager = shell.NewExecManager(func(e shell.ExecExit) {
		s.resources.tasks.Publish(execExitEvent(e))
	})
	s.freshness = fs.NewFreshness(ws.Root)
	s.schedules = schedule.NewFileStore(filepath.Join(artifactDir, "schedules"))

	shellOpts := &shell.Options{
		ScratchDir:         ws.ScratchPath,
		TempDir:            privateTemp,
		DisableSandbox:     a.cfg.DisableSandbox,
		Network:            a.cfg.SandboxNetwork,
		ExtraWritableRoots: []string{ws.MemoryPath},
	}
	allowedReadRoots = append(allowedReadRoots, privateTemp)
	allowedWriteRoots = append(allowedWriteRoots, privateTemp)
	s.fileReadRoots, s.fileWriteRoots = allowedReadRoots, allowedWriteRoots
	s.scriptOptions = &schedule.Options{WorkDir: ws.RootPath, Disabled: a.cfg.DisableShell, Elicitation: elicit, Shell: shellOpts}

	tools := ws.WithEditDiagnostics(fs.Tools(ws.Root, &fs.Options{
		AllowedReadRoots:  allowedReadRoots,
		AllowedWriteRoots: allowedWriteRoots,
		Freshness:         s.freshness,
	}))
	if !a.cfg.DisableShell {
		tools = append(tools, shell.ExecTools(s.resources.execManager, ws.RootPath, elicit, shellOpts)...)
	}
	tools = append(tools, schedule.Tools(s.schedules, s.scriptOptions)...)
	tools = append(tools, elicitation.Tools(elicit)...)
	if !a.cfg.DisableWebFetch {
		tools = append(tools, fetch.Tools(elicit)...)
	}
	if !a.cfg.DisableWebSearch {
		tools = append(tools, websearch.Tools(elicit)...)
	}
	s.tools = append(tools, subagent.Tools(sessionCfg, s.subagentContext, s.resources.tasks, subagent.Discover(ws.RootPath)...)...)
	return nil
}

func sessionFileRoots(ws *code.Workspace) (readRoots, writeRoots []string) {
	readRoots = skill.DiscoveryRoots(ws.RootPath)
	for _, sk := range ws.Skills() {
		if sk.Bundled {
			continue
		}
		if path := sk.AbsoluteDir(ws.RootPath); path != "" {
			readRoots = append(readRoots, path)
		}
	}
	// System skills live outside the workspace, but their references and scripts
	// must remain readable. Deliberately do not add this root to write roots.
	if ws.SystemSkillsPath != "" {
		readRoots = append(readRoots, ws.SystemSkillsPath)
	}
	readRoots = append(readRoots, ws.ScratchPath)

	if ws.MemoryPath != "" {
		readRoots = append(readRoots, ws.MemoryPath)
		writeRoots = append(writeRoots, ws.MemoryPath)
	}

	return readRoots, writeRoots
}
