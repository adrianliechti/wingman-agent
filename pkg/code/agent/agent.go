package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	elicittool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/elicit"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fetch"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/websearch"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

type Agent struct {
	workspace *code.Workspace
	cfg       Config

	uiMu    sync.RWMutex
	ui      code.UI
	prompts *tool.Elicitation

	lastActive atomic.Value

	sessionsDir string

	modelMu           sync.Mutex
	modelID           string
	effort            string
	complexModel      string
	utilityModel      string
	upstreamModels    map[string]bool
	modelDiscoveryErr error

	mu       sync.Mutex
	sessions map[string]*sessionState
	closed   bool
}

var _ code.Agent = (*Agent)(nil)

type sessionState struct {
	id      string
	parent  *Agent
	aa      *harness.Agent
	journal *session.Journal

	modelID string
	effort  *string // nil inherits the agent default; an empty value selects auto.

	// mode is switchable while a turn is running, so it is read from the turn
	// goroutine and written from the UI one.
	mode        atomic.Value // sessionMode
	unattended  atomic.Bool
	reviews     reviewBreaker
	toolSet     *tool.Set
	turnTools   atomic.Value // []tool.Tool, pinned for the running turn
	execManager *shell.ExecManager
	tasks       *task.Registry

	schedules     schedule.ObservableStore
	scriptOptions *schedule.Options

	freshness *fs.Freshness
	watchStop chan struct{}

	changedMu    sync.Mutex
	changedPaths []string

	hookContextMu sync.Mutex
	hookContext   []string

	projectInstructionsMu   sync.Mutex
	projectInstructionCache projectInstructionCache

	cancelMu    sync.Mutex
	cancelFn    context.CancelCauseFunc
	cancelGen   uint64
	closed      bool
	startSource string
}

type sessionMode string

const (
	modeAgent      sessionMode = code.AgentModeID
	modePlan       sessionMode = code.PlanModeID
	modeUnattended sessionMode = code.UnattendedModeID
)

func (s *sessionState) currentMode() sessionMode {
	if mode, ok := s.mode.Load().(sessionMode); ok && mode != "" {
		return mode
	}
	return modeAgent
}

func (s *sessionState) setMode(mode sessionMode) {
	// Retain the legacy unattended mode as a shortcut for existing callers.
	if mode == modeUnattended {
		s.unattended.Store(true)
		mode = modeAgent
	}
	s.mode.Store(mode)
}

func (s *sessionState) permissionMode() string {
	if s.currentMode() == modePlan {
		return "plan"
	}
	if s.unattended.Load() {
		return "bypassPermissions"
	}
	return "default"
}

// New creates a code agent; the caller keeps ownership of the workspace and telemetry.
func New(ws *code.Workspace, cfg Config, ui code.UI) *Agent {
	cfg.Config = *cfg.Config.Clone()
	a := &Agent{
		workspace:    ws,
		cfg:          cfg,
		ui:           ui,
		complexModel: cfg.ComplexModel,
		utilityModel: cfg.UtilityModel,
		sessionsDir:  filepath.Join(filepath.Dir(ws.MemoryPath), "sessions"),
		sessions:     map[string]*sessionState{},
	}
	if cfg.Model != nil {
		a.modelID = cfg.Model()
	}
	if cfg.Effort != nil {
		a.effort = cfg.Effort()
	}
	a.prompts = &tool.Elicitation{Elicit: a.elicit, Confirm: a.confirm, Review: a.review}

	// MCP servers elicit through the same UI surface as the elicit tool. Their
	// requests may arrive without session correlation. Web embedders require
	// an explicit session; single-client embedders retain their routing fallback.
	if ws.MCP != nil {
		ws.MCP.SetElicit(a.elicit)
		ws.MCP.SetTelemetry(cfg.Telemetry)
	}

	return a
}

func (a *Agent) anySessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.sessions {
		return id
	}
	return ""
}

func (a *Agent) Name() string               { return code.BuiltinAgentName }
func (a *Agent) Workspace() *code.Workspace { return a.workspace }

func (a *Agent) SetUI(ui code.UI) {
	a.uiMu.Lock()
	a.ui = ui
	a.uiMu.Unlock()
}

func (a *Agent) currentUI() code.UI {
	a.uiMu.RLock()
	defer a.uiMu.RUnlock()
	return a.ui
}

func (a *Agent) ListSessions(_ context.Context) ([]code.SessionInfo, error) {
	saved, err := session.List(a.sessionsDir)
	if err != nil {
		return nil, err
	}
	out := make([]code.SessionInfo, 0, len(saved))
	for _, s := range saved {
		out = append(out, code.SessionInfo{
			ID:        s.ID,
			Title:     s.Title,
			UpdatedAt: s.UpdatedAt,
		})
	}
	return out, nil
}

func (a *Agent) NewSession(_ context.Context) (string, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return "", errors.New("agent is closed")
	}
	a.mu.Unlock()

	id := uuid.NewString()
	s, err := a.buildSession(id)
	if err != nil {
		return "", err
	}
	s.startSource = "startup"
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		s.close()
		return "", errors.New("agent is closed")
	}
	a.sessions[id] = s
	a.mu.Unlock()
	return id, nil
}

func (a *Agent) LoadSession(_ context.Context, id string) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.New("agent is closed")
	}
	if _, ok := a.sessions[id]; ok {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	saved, err := session.Load(a.sessionsDir, id)
	if err != nil {
		return err
	}
	s, err := a.buildSession(id)
	if err != nil {
		return err
	}
	s.startSource = "resume"
	a.restoreSessionSettings(s, saved.State)
	if err := s.aa.Restore(saved.State); err != nil {
		s.close()
		return err
	}
	if err := s.aa.ReconcileInterrupted("process restarted before the operation settled"); err != nil {
		s.close()
		return err
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		s.close()
		return errors.New("agent is closed")
	}
	if _, loaded := a.sessions[id]; loaded {
		a.mu.Unlock()
		s.close()
		return nil
	}
	a.sessions[id] = s
	a.mu.Unlock()
	return nil
}

// ForkSession copies history, model and mode; tasks, schedules and queued inputs stay with the source.
func (a *Agent) ForkSession(ctx context.Context, id, title string) (string, error) {
	newID := uuid.NewString()
	source := a.session(id)
	if err := a.forkJournal(source, id, newID, title); err != nil {
		return "", err
	}
	err := a.copySessionSettings(id, newID)
	if err == nil {
		err = a.LoadSession(ctx, newID)
	}
	if err != nil {
		_ = session.Delete(a.sessionsDir, newID)
		return "", err
	}
	if fork := a.session(newID); fork != nil && source != nil {
		fork.setMode(source.currentMode())
		fork.unattended.Store(source.unattended.Load())
	}
	return newID, nil
}

func (a *Agent) forkJournal(source *sessionState, id, newID, title string) error {
	if source == nil {
		return session.Fork(a.sessionsDir, id, newID, title)
	}
	// Claiming the session keeps a turn from starting while the journal is copied.
	gen, err := source.claimTurn(func(error) {})
	if err != nil {
		return err
	}
	defer source.clearCancel(gen)
	return session.Fork(a.sessionsDir, id, newID, title)
}

func (a *Agent) DeleteSession(_ context.Context, id string) error {
	a.mu.Lock()
	s, inMem := a.sessions[id]
	if inMem {
		delete(a.sessions, id)
	}
	a.mu.Unlock()

	if inMem {
		s.close()
	}
	if err := session.Delete(a.sessionsDir, id); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *Agent) Save(id string) error {
	a.mu.Lock()
	s, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return nil
	}
	return session.Save(a.sessionsDir, id, s.aa.StateSnapshot())
}

func (a *Agent) SessionsDir() string { return a.sessionsDir }

func (a *Agent) Messages(id string) []harness.Message {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.aa.MessagesSnapshot()
}

func (a *Agent) HistorySnapshot(id string) code.HistorySnapshot {
	s := a.session(id)
	if s == nil {
		return code.HistorySnapshot{}
	}
	state := s.aa.StateSnapshot()
	return code.HistorySnapshot{Messages: state.Messages, Revision: state.Revision}
}

func (a *Agent) HistoryVersion(id string) code.HistoryVersion {
	s := a.session(id)
	if s == nil {
		return code.HistoryVersion{}
	}
	messageCount, revision := s.aa.StateVersion()
	return code.HistoryVersion{Revision: revision, MessageCount: messageCount}
}

func (a *Agent) Recap(ctx context.Context, id string) (string, error) {
	s := a.session(id)
	if s == nil {
		return "", fmt.Errorf("session %s not found", id)
	}
	return s.aa.Recap(ctx)
}

func (a *Agent) Compact(ctx context.Context, id, focus string) (harness.CompactResult, error) {
	s := a.session(id)
	if s == nil {
		return harness.CompactResult{}, fmt.Errorf("session %s not found", id)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	gen, err := s.claimTurn(cancel)
	if err != nil {
		return harness.CompactResult{}, err
	}
	defer s.clearCancel(gen)
	return s.aa.Compact(hook.WithRuntime(ctx, s.hookRuntime()), focus)
}

func (a *Agent) ContextStats(id string) (harness.ContextStats, bool) {
	s := a.session(id)
	if s == nil {
		return harness.ContextStats{}, false
	}
	return s.aa.ContextStats(), true
}

func (a *Agent) Usage(id string) harness.Usage {
	s := a.session(id)
	if s == nil {
		return harness.Usage{}
	}
	return s.aa.UsageSnapshot()
}

func (a *Agent) session(id string) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *Agent) HasSession(id string) bool { return a.session(id) != nil }

// Tasks exposes the session's background-agent registry so UI surfaces can
// list running agents and deliver completion notifications.
func (a *Agent) Tasks(id string) *task.Registry {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.tasks
}

// ExecSessions exposes commands that exec left running so web clients
// can present them with the session's other background work.
func (a *Agent) ExecSessions(id string) []shell.ExecSessionInfo {
	s := a.session(id)
	if s == nil || s.execManager == nil {
		return nil
	}
	return s.execManager.BackgroundSessions()
}

func (a *Agent) ExecSession(id string, execID int) (shell.ExecSessionInfo, bool) {
	s := a.session(id)
	if s == nil || s.execManager == nil {
		return shell.ExecSessionInfo{}, false
	}
	return s.execManager.BackgroundSession(execID)
}

func (a *Agent) StopExecSession(id string, execID int) error {
	s := a.session(id)
	if s == nil || s.execManager == nil {
		return fmt.Errorf("session not found")
	}
	return s.execManager.StopBackgroundSession(execID)
}

// RecordUserShellCommand records a user-executed `!` shell command and its
// output in the session conversation history so the model can see it on
// subsequent turns.
func (a *Agent) RecordUserShellCommand(id, command, output string, exitCode int, duration time.Duration) error {
	s := a.session(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	msg := harness.Message{
		Role:    harness.RoleUser,
		Content: []harness.Content{{Text: code.FormatUserShellCommand(command, output, exitCode, duration)}},
	}
	if err := s.aa.RecordMessage(msg); err != nil {
		return err
	}
	return a.Save(id)
}

// Schedules exposes the session's scheduled tasks so UI surfaces can list
// them alongside its background agents.
func (a *Agent) Schedules(id string) schedule.Store {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.schedules
}

// RunningTaskCount sums running background agents across every live session,
// so quitting warns about agents outside the currently viewed session too.
func (a *Agent) RunningTaskCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	total := 0
	for _, s := range a.sessions {
		running, _ := s.tasks.Counts()
		total += running + len(s.execManager.BackgroundSessions())
	}
	return total
}

func (a *Agent) Send(ctx context.Context, id string, input []harness.Content) (iter.Seq2[harness.Message, error], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, code.ErrEmptyInput
	}
	a.mu.Lock()
	s, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found; call NewSession first", id)
	}

	a.lastActive.Store(id)

	sendCtx, cancel := context.WithCancelCause(code.WithSessionID(ctx, id))
	// Startup cannot ask a web client without session correlation. Defer local
	// configuration approval until this turn, where prompts have an owner.
	if a.workspace.MCP != nil && len(a.workspace.MCP.TrustRequired) > 0 {
		if err := a.workspace.InitMCP(sendCtx, a.currentUI()); err != nil {
			if sendCtx.Err() != nil {
				cancel(nil)
				return nil, sendCtx.Err()
			}
			input = append(slices.Clone(input), harness.Content{Text: "Some MCP servers are unavailable: " + err.Error(), Hidden: true})
		}
	}
	stream, gen, err := s.beginSend(sendCtx, input, cancel)
	if err != nil {
		cancel(nil)
		return nil, err
	}

	return func(yield func(harness.Message, error) bool) {
		defer func() {
			s.clearCancel(gen)
			cancel(nil)
		}()
		for msg, err := range stream {
			if !yield(msg, err) {
				return
			}
		}
	}, nil
}

func (a *Agent) TurnFeatures(string) code.TurnFeatures {
	return code.TurnFeatures{Steer: true}
}

func (a *Agent) Steer(_ context.Context, id string, input code.TurnInput) error {
	s := a.session(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	if !s.aa.QueueInputWithID(input.Content, input.ID) {
		return code.ErrNoActiveTurn
	}
	return nil
}

func (a *Agent) Cancel(id string) {
	if s := a.session(id); s != nil {
		s.cancel(harness.ErrInterrupted)
	}
}

func (a *Agent) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := slices.Collect(maps.Values(a.sessions))
	a.sessions = map[string]*sessionState{}
	a.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	return nil
}

var wingmanModes = []code.Mode{
	{ID: code.AgentModeID, Name: "Agent", Description: "Make changes and run checks. Risky actions are reviewed automatically."},
	{ID: code.PlanModeID, Name: "Plan", Description: "Read-only. Proposes a plan."},
}

func (a *Agent) Modes(sessionID string) ([]code.Mode, string) {
	out := make([]code.Mode, len(wingmanModes))
	copy(out, wingmanModes)
	current := code.AgentModeID
	if s := a.session(sessionID); s != nil {
		current = string(s.currentMode())
	}
	return out, current
}

func (a *Agent) SetMode(_ context.Context, sessionID, modeID string) error {
	var mode sessionMode
	switch modeID {
	case code.AgentModeID:
		mode = modeAgent
	case code.PlanModeID:
		mode = modePlan
	case code.UnattendedModeID:
		mode = modeUnattended
	default:
		return fmt.Errorf("unknown mode %q", modeID)
	}
	if s := a.session(sessionID); s != nil {
		s.setMode(mode)
	}
	return nil
}

func (a *Agent) Unattended(sessionID string) bool {
	if s := a.session(sessionID); s != nil {
		return s.unattended.Load()
	}
	return false
}

func (a *Agent) SetUnattended(_ context.Context, sessionID string, enabled bool) error {
	if s := a.session(sessionID); s != nil {
		s.unattended.Store(enabled)
	}
	return nil
}

func (a *Agent) Tools(id string) []tool.Tool {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.tools()
}

func (a *Agent) buildSession(id string) (*sessionState, error) {
	journal, err := session.OpenJournal(a.sessionsDir, id)
	if err != nil {
		return nil, err
	}
	sessionCfg := a.cfg.Clone()
	s := &sessionState{
		id:      id,
		parent:  a,
		aa:      &harness.Agent{Config: sessionCfg, Recorder: journal},
		journal: journal,
	}
	s.aa.CacheKey = id
	s.setMode(modeAgent)
	sessionCfg.Tools = s.tools
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
	elicit := a.prompts
	ws := a.workspace

	globalHooks, globalErr := external.Load(userHooksConfigPath())
	workspaceHooksPath := filepath.Join(ws.RootPath, ".codex", "hooks.json")
	workspaceHooks, workspaceErr := external.Load(workspaceHooksPath)

	// Fail closed: a hook config the user wrote but that no longer parses must
	// not silently disable the guards it configures.
	if err := errors.Join(globalErr, workspaceErr); err != nil {
		message := fmt.Sprintf("hook configuration is invalid; fix or remove it to unblock tools: %v", err)
		sessionCfg.Hooks.PreToolUse = append(sessionCfg.Hooks.PreToolUse,
			func(context.Context, tool.ToolCall) (hook.PreToolUseOutcome, error) {
				return hook.PreToolUseOutcome{Outcome: hook.Outcome{Block: true, Reason: message}}, nil
			},
		)
	}

	hookAsync := external.NewAsync(func(runtime hook.Runtime, parts []string) {
		// A subagent's hook context describes calls the main turn never saw.
		if runtime.AgentID != "" {
			return
		}
		if !s.aa.QueueInput([]harness.Content{{Text: strings.Join(parts, "\n\n"), Hidden: true}}) {
			s.stashHookContext(parts)
		}
	})
	hookOptions := external.BuildOptions{Async: hookAsync, MCP: ws.CallMCPTool, SpillDir: ws.ScratchPath}
	hookTrust := attendedTrust{TrustStore: ws.HookTrust(), session: s}

	// Workspace hooks come with the repo, not the user — gate them behind a
	// confirmation so a cloned project cannot run commands unprompted.
	workspaceOptions := hookOptions
	if rules := workspaceHooks.RuleCount(); rules > 0 {
		fingerprint, _ := workspaceHooks.Fingerprint(ws.RootPath, workspaceHooksPath)
		workspaceOptions.Gate = &external.Gate{
			Confirm:     a.confirm,
			Message:     fmt.Sprintf("Run %d workspace hook rule(s) from %s?\n\nApproval is remembered until these hooks change.", rules, workspaceHooksPath),
			Fingerprint: func() string { return fingerprint },
			Store:       hookTrust,
		}
	}

	sessionCfg.Hooks.Append(globalHooks.BuildWithOptions(ws.RootPath, hookOptions))
	sessionCfg.Hooks.Append(workspaceHooks.BuildWithOptions(ws.RootPath, workspaceOptions))
	for i := range ws.Plugins {
		p := &ws.Plugins[i]
		if p.Hooks == nil || p.Hooks.RuleCount() == 0 {
			continue
		}
		declarations, declarationsErr := p.Hooks.Fingerprint(ws.RootPath, p.Name, p.Root, p.Data)
		pluginOptions := hookOptions
		pluginOptions.Gate = &external.Gate{
			Confirm: a.confirm,
			Message: fmt.Sprintf("Run %d hook rule(s) from plugin %q at %s?\n\nApproval is remembered until the plugin changes.", p.Hooks.RuleCount(), p.Name, p.Root),
			Fingerprint: func() string {
				content, err := external.ContentHash(p.Root)
				if declarationsErr != nil || err != nil {
					return ""
				}
				return declarations + "/" + content
			},
			Store: hookTrust,
		}
		pluginOptions.Environment = map[string]string{
			"PLUGIN_ROOT":        p.Root,
			"PLUGIN_DATA":        p.Data,
			"CLAUDE_PLUGIN_ROOT": p.Root,
			"CLAUDE_PLUGIN_DATA": p.Data,
		}
		sessionCfg.Hooks.Append(p.Hooks.BuildWithOptions(ws.RootPath, pluginOptions))
	}
	sessionCfg.Hooks.TransformToolResult = append(sessionCfg.Hooks.TransformToolResult,
		truncation.Transform(ws.ScratchPath),
	)

	allowedReadRoots, allowedWriteRoots := sessionFileRoots(ws)
	if !a.cfg.DisableShell && !a.cfg.DisableSandbox {
		if err := shell.CheckSandbox(ws.RootPath); err != nil {
			return nil, fmt.Errorf("sandbox unavailable (WINGMAN_SANDBOX=off explicitly disables isolation): %w", err)
		}
	}

	artifactDir, err := session.ArtifactDir(a.sessionsDir, id)
	if err != nil {
		return nil, err
	}
	s.tasks, err = task.NewFileRegistry(filepath.Join(artifactDir, "tasks.json"))
	if err != nil {
		return nil, fmt.Errorf("restore background tasks: %w", err)
	}
	s.execManager = shell.NewExecManager(func(e shell.ExecExit) {
		s.tasks.Publish(execExitEvent(e))
	})
	s.freshness = fs.NewFreshness(ws.Root)
	s.schedules = schedule.NewFileStore(filepath.Join(artifactDir, "schedules"))
	s.watchStop = make(chan struct{})

	sessionCfg.Hooks.UserPromptSubmit = append(sessionCfg.Hooks.UserPromptSubmit,
		func(context.Context, string) (hook.Outcome, error) {
			s.sweepFileChanges()
			parts := s.takeHookContext()
			if notice := formatFileChangeNotice(s.takeFileChanges()); notice != "" {
				parts = append(parts, notice)
			}
			return hook.Outcome{AdditionalContext: parts}, nil
		},
	)

	privateTemp, err := os.MkdirTemp("", "wingman-session-")
	if err != nil {
		return nil, fmt.Errorf("create private session temp: %w", err)
	}
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
	go s.watchFileChanges()
	go s.runSchedules()

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
	s.toolSet.Own("async hooks", func() error {
		hookAsync.Close()
		return nil
	})
	return s, nil
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

// execExitNotifyLimit caps how much trailing output of a backgrounded command
// an exit notification injects into the conversation.
const execExitNotifyLimit = 16 * 1024

func execExitEvent(e shell.ExecExit) task.Event {
	description := e.Description
	if description == "" {
		description = text.TruncateHead(e.Command, 80)
	}

	status := task.StatusDone
	if e.Failed {
		status = task.StatusFailed
	}

	result := e.Notice
	if out := strings.TrimSpace(e.Output); out != "" {
		if len(out) > execExitNotifyLimit {
			out = "[earlier output truncated]\n" + text.TailLines(out, execExitNotifyLimit)
		}
		result += "\n\nOutput:\n" + out
	}

	return task.Event{
		ID:          fmt.Sprintf("exec-%d", e.SessionID),
		Kind:        task.KindCommand,
		AgentType:   "exec",
		Description: description,
		Seq:         1,
		Status:      status,
		Result:      result,
		Elapsed:     e.Elapsed,
	}
}

func userHooksConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

func (a *Agent) promptContext(ctx context.Context) context.Context {
	if code.SessionIDFromContext(ctx) != "" {
		return ctx
	}
	if sid := hook.RuntimeFromContext(ctx).SessionID; sid != "" && a.session(sid) != nil {
		return code.WithSessionID(ctx, sid)
	}
	if a.cfg.IsolateSessions {
		return ctx
	}

	sid, _ := a.lastActive.Load().(string)
	if sid != "" && a.session(sid) == nil {
		sid = ""
	}
	if sid == "" {
		sid = a.anySessionID()
	}
	if sid == "" {
		return ctx
	}
	return code.WithSessionID(ctx, sid)
}

func (a *Agent) elicit(ctx context.Context, req tool.ElicitRequest) (tool.ElicitResult, error) {
	ctx = a.promptContext(ctx)
	if s := a.session(code.SessionIDFromContext(ctx)); s != nil && s.unattended.Load() {
		return code.UnattendedElicitation(req), nil
	}
	ui := a.currentUI()
	if ui == nil {
		return tool.ElicitResult{Action: tool.ElicitCancel}, nil
	}
	return ui.Elicit(ctx, req)
}

// confirm asks a person, or approves in unattended mode; tool approvals go through review.
func (a *Agent) confirm(ctx context.Context, message string) (bool, error) {
	ctx = a.promptContext(ctx)
	if s := a.session(code.SessionIDFromContext(ctx)); s != nil && s.unattended.Load() {
		return true, nil
	}
	ui := a.currentUI()
	if ui == nil {
		return false, nil
	}
	return ui.Confirm(ctx, message)
}

func (s *sessionState) beginSend(ctx context.Context, input []harness.Content, cancel context.CancelCauseFunc) (iter.Seq2[harness.Message, error], uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	catalog := collectManagedTools(s.parent.workspace)
	// Claiming before setup lets Cancel and Close reach hooks that wait on UI or external processes.
	gen, err := s.claimTurn(cancel)
	if err != nil {
		return nil, 0, err
	}
	s.turnTools.Store(catalog)
	stream, err := s.aa.Send(hook.WithRuntime(ctx, s.hookRuntime()), input)
	if err != nil {
		s.clearCancel(gen)
		return nil, 0, err
	}
	if stream == nil {
		s.clearCancel(gen)
		return nil, 0, errors.New("agent returned a nil turn stream")
	}
	return stream, gen, nil
}

// claimTurn reserves the session for one operation; release it with clearCancel.
func (s *sessionState) claimTurn(cancel context.CancelCauseFunc) (uint64, error) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.closed {
		return 0, errors.New("session is closed")
	}
	if s.cancelFn != nil || s.aa.Running() {
		return 0, code.ErrTurnInProgress
	}
	s.cancelGen++
	s.cancelFn = cancel
	return s.cancelGen, nil
}

func (s *sessionState) hookRuntime() hook.Runtime {
	return hook.Runtime{
		SessionID:      s.aa.CacheKey,
		CWD:            s.parent.workspace.RootPath,
		Model:          s.aa.Model(),
		PermissionMode: s.permissionMode(),
		StartSource:    s.startSource,
	}
}

func (s *sessionState) clearCancel(gen uint64) {
	s.cancelMu.Lock()
	last := s.cancelGen == gen
	if last {
		s.cancelFn = nil
		s.turnTools.Store([]tool.Tool(nil))
	}
	s.cancelMu.Unlock()
}

func (s *sessionState) cancel(cause error) {
	s.cancelMu.Lock()
	fn := s.cancelFn
	s.cancelMu.Unlock()
	if fn != nil {
		fn(cause)
	}
}

func (s *sessionState) close() {
	s.cancelMu.Lock()
	if s.closed {
		s.cancelMu.Unlock()
		return
	}
	s.closed = true
	s.cancelGen++
	fn := s.cancelFn
	s.cancelFn = nil
	s.cancelMu.Unlock()
	if fn != nil {
		fn(nil)
	}
	close(s.watchStop)
	_ = s.toolSet.Close()

	if hooks := s.aa.Hooks.SessionEnd; len(hooks) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = hook.WithRuntime(ctx, hook.Runtime{
			SessionID:      s.aa.CacheKey,
			CWD:            s.parent.workspace.RootPath,
			Model:          s.aa.Model(),
			PermissionMode: "default",
		})
		for _, h := range hooks {
			h(ctx, "other")
		}
	}
}

// watchFileChanges announces external file modifications mid-turn: while a
// turn runs, tracked files that changed outside the session's file tools are
// reported to the model as hidden context at the next model boundary. Idle
// periods are swept lazily by the UserPromptSubmit hook instead.
func (s *sessionState) watchFileChanges() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.watchStop:
			return
		case <-ticker.C:
			if !s.aa.Running() {
				continue
			}
			s.sweepFileChanges()
			paths := s.takeFileChanges()
			if len(paths) == 0 {
				continue
			}
			notice := formatFileChangeNotice(paths)
			if !s.aa.QueueInput([]harness.Content{{Text: notice, Hidden: true}}) {
				s.stashFileChanges(paths)
			}
		}
	}
}

func (s *sessionState) sweepFileChanges() {
	if changed := s.freshness.Changed(); len(changed) > 0 {
		s.stashFileChanges(changed)
	}
}

func (s *sessionState) stashFileChanges(paths []string) {
	s.changedMu.Lock()
	for _, p := range paths {
		if !slices.Contains(s.changedPaths, p) {
			s.changedPaths = append(s.changedPaths, p)
		}
	}
	s.changedMu.Unlock()
}

// stashHookContext holds async hook output that finished between turns for the next prompt.
func (s *sessionState) stashHookContext(parts []string) {
	s.hookContextMu.Lock()
	s.hookContext = append(s.hookContext, parts...)
	s.hookContextMu.Unlock()
}

func (s *sessionState) takeHookContext() []string {
	s.hookContextMu.Lock()
	parts := s.hookContext
	s.hookContext = nil
	s.hookContextMu.Unlock()
	return parts
}

// attendedTrust remembers only approvals a person gave; unattended ones last for the session.
type attendedTrust struct {
	external.TrustStore
	session *sessionState
}

func (t attendedTrust) Trust(fingerprint string) error {
	if t.session.unattended.Load() {
		return nil
	}
	return t.TrustStore.Trust(fingerprint)
}

const fileChangeNoticeMax = 20

func (s *sessionState) takeFileChanges() []string {
	s.changedMu.Lock()
	paths := s.changedPaths
	s.changedPaths = nil
	s.changedMu.Unlock()
	return paths
}

func formatFileChangeNotice(paths []string) string {
	if len(paths) == 0 {
		return ""
	}

	listed := paths
	suffix := ""
	if len(listed) > fileChangeNoticeMax {
		suffix = fmt.Sprintf(" (and %d more)", len(listed)-fileChangeNoticeMax)
		listed = listed[:fileChangeNoticeMax]
	}

	return fmt.Sprintf("<system-reminder>These files changed on disk outside this session's file tools (edited by the user, a linter, or a shell command): %s%s. Your memory of their content is stale — re-read them before editing, take the external changes into account, and never revert them.</system-reminder>", strings.Join(listed, ", "), suffix)
}

func (s *sessionState) tools() []tool.Tool {
	tools := s.toolSet.Slice()
	tools = append(tools, s.managedTools()...)
	for i, t := range tools {
		if t.Name == "lsp" {
			tools[i] = s.freshness.TrackSnapshots(t)
		}
	}
	if s.currentMode() == modePlan {
		tools = planModeTools(tools)
	}
	if s.unattended.Load() {
		tools = slices.DeleteFunc(tools, func(t tool.Tool) bool { return t.Name == "elicit" || t.Name == "ask" })
	}
	slices.SortStableFunc(tools, func(a, b tool.Tool) int { return cmp.Compare(a.Name, b.Name) })
	return tools
}

// managedTools stays pinned for the length of a turn: the harness asks for the
// tool set once per round so a mode switch lands mid-turn, and an MCP
// tools/list_changed arriving between rounds must not reshape the catalog under
// the running turn.
func (s *sessionState) managedTools() []tool.Tool {
	if pinned, ok := s.turnTools.Load().([]tool.Tool); ok && pinned != nil {
		return pinned
	}
	return collectManagedTools(s.parent.workspace)
}

func collectManagedTools(ws *code.Workspace) []tool.Tool {
	mcpTools, lspTools, graphTools := ws.ManagedTools()
	return slices.Concat(mcpTools, lspTools, graphTools)
}

func planModeTools(tools []tool.Tool) []tool.Tool {
	filtered := make([]tool.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Effect == nil {
			continue
		}
		switch t.Effect(nil) {
		case tool.EffectReadOnly:
			filtered = append(filtered, t)
		case tool.EffectDynamic:
			t.Execute = planModeEffectExecute(t)
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func planModeEffectExecute(t tool.Tool) func(context.Context, map[string]any) (tool.Result, error) {
	return func(ctx context.Context, args map[string]any) (tool.Result, error) {
		if t.Effect == nil || t.Effect(args) != tool.EffectReadOnly {
			return tool.Result{}, fmt.Errorf("plan mode only allows read-only tool calls")
		}
		return t.Execute(ctx, args)
	}
}

func (s *sessionState) instructions() string {
	option, _ := s.parent.roleModel(s, "")
	base, data := instructionTemplate(option.ID, prompt.SectionData{
		ToolNames:      promptToolNames(s.tools()),
		PlanMode:       s.currentMode() == modePlan,
		UnattendedMode: s.unattended.Load(),
	})
	return prompt.BuildBaseInstructions(base, data)
}

func (s *sessionState) contextInstructions() string {
	return prompt.BuildSessionContext(s.instructionsData())
}

func (s *sessionState) subagentContext(tools []tool.Tool) string {
	_, data := instructionTemplate("", s.instructionsData())
	data.ToolNames = promptToolNames(tools)
	return prompt.BuildAgentContext(data)
}

func promptToolNames(tools []tool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func BuildInstructions(modelID string, data prompt.SectionData) string {
	base, data := instructionTemplate(modelID, data)
	return prompt.BuildInstructions(base, data)
}

func instructionTemplate(modelID string, data prompt.SectionData) (string, prompt.SectionData) {
	variant := prompt.VariantFor(modelID)
	selected, ok := model.Find(modelID)
	if !ok {
		selected.ID = modelID
		selected.Name = modelID
	}
	data.Model = selected

	base := strings.TrimSpace(variant.Agent)
	var modes []string
	if data.PlanMode {
		modes = append(modes, variant.Plan)
	}
	if data.UnattendedMode {
		modes = append(modes, variant.Unattended)
	}
	data.ModeInstructions = strings.Join(modes, "\n\n")
	return base, data
}

func (s *sessionState) instructionsData() prompt.SectionData {
	ws := s.parent.workspace
	now := time.Now()
	return prompt.SectionData{
		ToolNames:           promptToolNames(s.tools()),
		PlanMode:            s.currentMode() == modePlan,
		UnattendedMode:      s.unattended.Load(),
		Date:                now.Format("2006-01-02"),
		Timezone:            localTimezone(now),
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		WorkingDir:          ws.RootPath,
		Shell:               localShell(),
		MemoryDir:           ws.MemoryPath,
		MemoryContent:       ws.MemoryContent(),
		Skills:              skill.FormatForPrompt(ws.Skills()),
		ProjectInstructions: s.projectInstructions(),
	}
}

func localShell() string {
	for _, name := range []string{"SHELL", "COMSPEC"} {
		if shell := strings.TrimSpace(os.Getenv(name)); shell != "" {
			return filepath.Base(shell)
		}
	}
	return ""
}

func localTimezone(now time.Time) string {
	if timezone := strings.TrimPrefix(strings.TrimSpace(os.Getenv("TZ")), ":"); timezone != "" && !filepath.IsAbs(timezone) {
		return timezone
	}
	if timezone := now.Location().String(); timezone != "" && timezone != "Local" {
		return timezone
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		const marker = "/zoneinfo/"
		normalized := filepath.ToSlash(target)
		if index := strings.LastIndex(normalized, marker); index >= 0 {
			return normalized[index+len(marker):]
		}
	}

	name, offset := now.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s (UTC%s%02d:%02d)", name, sign, offset/3600, offset%3600/60)
}

// requiresFinish enables the explicit finish_turn protocol for Claude models,
// whose native stop reasons carry no message phase. The catalog is authoritative
// for known models. Unknown deployment names can identify a model family as a
// complete word, including Bedrock ARNs and aliases such as "bedrock-opus-5-5".
func requiresFinish(id string) bool {
	if m, ok := model.Find(id); ok {
		return strings.EqualFold(m.Namespace, "anthropic")
	}
	for _, part := range strings.FieldsFunc(strings.ToLower(id), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		switch part {
		case "claude", "opus", "sonnet", "haiku", "fable", "mythos":
			return true
		}
	}
	return false
}
