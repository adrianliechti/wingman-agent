package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	elicittool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/elicit"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fetch"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/websearch"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

func (a *Agent) buildSession(id string) (*sessionState, error) {
	journal, err := session.OpenJournal(a.sessionsDir, id)
	if err != nil {
		return nil, err
	}
	s := &sessionState{
		id:      id,
		parent:  a,
		aa:      &harness.Agent{Config: a.cfg.Clone(), Recorder: journal},
		journal: journal,
	}
	s.configureHarness()
	// Restoring subagent tools derives child configs immediately, so hooks
	// must be complete before constructing the catalog.
	closeHooks := s.configureHooks()
	if err := s.configureTools(); err != nil {
		return nil, errors.Join(err, closeHooks())
	}
	s.toolSet.Own("async hooks", closeHooks)
	s.configureLifecycle()
	return s, nil
}

// configureHarness binds the session's live host policy before tools capture it.
func (s *sessionState) configureHarness() {
	a, sessionCfg := s.parent, s.aa.Config
	s.aa.CacheKey = s.id
	s.setMode(modeAgent)
	sessionCfg.Tools = s.catalogTools
	sessionCfg.PinTools = true
	sessionCfg.FilterTools = s.filterTools
	prepare := sessionCfg.PrepareTurn
	sessionCfg.PrepareTurn = func(ctx context.Context, input []harness.Content) ([]harness.Content, error) {
		if prepare != nil {
			var err error
			input, err = prepare(ctx, input)
			if err != nil {
				return nil, err
			}
		}
		return s.prepareTurn(ctx, input)
	}
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

// configureTools acquires session resources and transfers ownership to toolSet.
// All fallible setup precedes tool publication; failed restore removes the temp directory.
func (s *sessionState) configureTools() error {
	a, sessionCfg := s.parent, s.aa.Config
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
	s.tasks, err = task.NewFileRegistry(filepath.Join(artifactDir, "tasks.json"))
	if err != nil {
		return errors.Join(fmt.Errorf("restore background tasks: %w", err), os.RemoveAll(privateTemp))
	}
	s.execManager = shell.NewExecManager(func(e shell.ExecExit) {
		s.tasks.Publish(execExitEvent(e))
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
	s.scriptOptions = &schedule.Options{WorkDir: ws.RootPath, Disabled: a.cfg.DisableShell, Elicitation: elicit, Shell: shellOpts}

	var shellTools []tool.Tool
	if !a.cfg.DisableShell {
		shellTools = shell.ExecTools(s.execManager, ws.RootPath, elicit, shellOpts)
	}
	var webFetchTools []tool.Tool
	if !a.cfg.DisableWebFetch {
		webFetchTools = fetch.Tools(elicit)
	}
	var webSearchTools []tool.Tool
	if !a.cfg.DisableWebSearch {
		webSearchTools = websearch.Tools(elicit)
	}

	baseTools := slices.Concat(
		ws.WithEditDiagnostics(fs.Tools(ws.Root, &fs.Options{
			AllowedReadRoots:  allowedReadRoots,
			AllowedWriteRoots: allowedWriteRoots,
			Freshness:         s.freshness,
		})),
		shellTools,
		schedule.Tools(s.schedules, s.scriptOptions),
		elicittool.Tools(elicit),
		webFetchTools,
		webSearchTools,
		subagent.Tools(sessionCfg, s.subagentContext, s.tasks, subagent.Discover(ws.RootPath)...),
	)
	s.toolSet = tool.NewSet(baseTools...)
	// Close runs in reverse: tasks first so background subagents are canceled
	// before their shared exec sessions are killed.
	s.toolSet.Own("private temporary files", func() error { return os.RemoveAll(privateTemp) })
	s.toolSet.Own("exec manager", func() error {
		s.execManager.Close()
		return nil
	})
	s.toolSet.Own("task registry", func() error {
		s.tasks.Close()
		return nil
	})
	return nil
}

// configureLifecycle starts workers only after registration and joins them before cleanup.
func (s *sessionState) configureLifecycle() {
	sessionCfg := s.aa.Config
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	var background sync.WaitGroup
	s.startBackground = func() {
		background.Go(func() { s.watchFileChanges(backgroundCtx) })
		background.Go(func() { s.scheduleRunner().Run(backgroundCtx) })
	}
	closeSession := sessionCfg.CloseSession
	sessionCfg.CloseSession = func() error {
		cancelBackground()
		background.Wait()
		err := s.toolSet.Close()
		if closeSession != nil {
			err = errors.Join(err, closeSession())
		}
		return err
	}
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
