package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
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
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

type Agent struct {
	workspace *code.Workspace
	cfg       Config

	ui      code.UI
	prompts *tool.Elicitation

	sessionsDir string

	modelMu        sync.Mutex
	modelID        string
	effort         string
	complexModel   string
	utilityModel   string
	upstreamModels map[string]bool

	mu           sync.Mutex // Protects ui and session bookkeeping.
	sessions     map[string]*sessionState
	lastActive   string
	closed       bool
	sessionLocks map[string]*sessionLock
}

var _ code.Agent = (*Agent)(nil)

type sessionState struct {
	resources sessionResources

	id      string
	parent  *Agent
	harness *harness.Agent

	modelID string
	effort  *string // nil inherits the agent default; an empty value selects auto.

	// Plan mode can change while a turn is running.
	planMode   atomic.Bool
	unattended atomic.Bool
	reviews    reviewBreaker
	tools      []tool.Tool

	schedules     schedule.ObservableStore
	scriptOptions *schedule.Options

	freshness      *fs.Freshness
	fileReadRoots  []string
	fileWriteRoots []string

	changedMu    sync.Mutex
	changedPaths []string

	hookContextMu sync.Mutex
	hookContext   []string

	projectInstructionsMu   sync.Mutex
	projectInstructionCache projectInstructionCache

	startSource string
}

type sessionMode string

const (
	modeAgent      sessionMode = code.AgentModeID
	modePlan       sessionMode = code.PlanModeID
	modeUnattended sessionMode = code.UnattendedModeID
)

func (s *sessionState) currentMode() sessionMode {
	if s.planMode.Load() {
		return modePlan
	}
	return modeAgent
}

func (s *sessionState) setMode(mode sessionMode) {
	// Retain the legacy unattended mode as a shortcut for existing callers.
	if mode == modeUnattended {
		s.unattended.Store(true)
	}
	s.planMode.Store(mode == modePlan)
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
		ws.SetMCPApproval(a.prompts)
	}

	return a
}

func (a *Agent) Name() string               { return code.BuiltinAgentName }
func (a *Agent) Workspace() *code.Workspace { return a.workspace }

func (a *Agent) SetUI(ui code.UI) {
	a.mu.Lock()
	a.ui = ui
	a.mu.Unlock()
}

func (a *Agent) currentUI() code.UI {
	a.mu.Lock()
	defer a.mu.Unlock()
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

func (a *Agent) NewSession(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
	if err := a.registerSession(ctx, s); err != nil {
		return "", err
	}
	return id, nil
}

func (a *Agent) LoadSession(ctx context.Context, id string) error {
	unlock, err := a.lockSession(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return a.loadSession(ctx, id)
}

func (a *Agent) loadSession(ctx context.Context, id string) error {
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
	if err := s.harness.Restore(saved.State); err != nil {
		return errors.Join(err, s.close())
	}
	if err := s.harness.ReconcileInterrupted("process restarted before the operation settled"); err != nil {
		return errors.Join(err, s.close())
	}

	return a.registerSession(ctx, s)
}

// registerSession publishes fully restored state before enabling background
// work. Holding mu across startup also keeps Close from overtaking worker Add.
func (a *Agent) registerSession(ctx context.Context, s *sessionState) error {
	a.mu.Lock()
	err := ctx.Err()
	if err == nil && a.closed {
		err = errors.New("agent is closed")
	}
	if err != nil {
		a.mu.Unlock()
		return errors.Join(err, s.close())
	}
	a.sessions[s.id] = s
	if err := s.harness.Go(s.watchFileChanges, s.scheduleRunner().Run); err != nil {
		delete(a.sessions, s.id)
		a.mu.Unlock()
		return errors.Join(err, s.close())
	}
	a.mu.Unlock()
	return nil
}

// ForkSession copies history, model and mode; tasks, schedules and queued inputs stay with the source.
func (a *Agent) ForkSession(ctx context.Context, id, title string) (string, error) {
	return a.fork(ctx, id, func(source *sessionState, newID string) (bool, error) {
		return a.forkJournal(ctx, source, id, newID, title)
	})
}

// ForkSessionThrough forks history through the turn of one user input. Only
// committed history is copied, so the source may keep running.
func (a *Agent) ForkSessionThrough(ctx context.Context, id, inputID, title string) (string, error) {
	return a.fork(ctx, id, func(_ *sessionState, newID string) (bool, error) {
		return false, session.ForkThrough(a.sessionsDir, id, newID, title, inputID)
	})
}

func (a *Agent) fork(ctx context.Context, id string, journal func(source *sessionState, newID string) (bool, error)) (string, error) {
	newID := uuid.NewString()
	source := a.session(id)
	empty, err := journal(source, newID)
	if err != nil {
		return "", err
	}
	err = a.copySessionSettings(id, newID)
	if err == nil {
		if empty {
			var fork *sessionState
			fork, err = a.buildSession(newID)
			if err == nil {
				a.restoreSessionSettings(fork, harness.State{})
				err = a.registerSession(ctx, fork)
			}
		} else {
			err = a.LoadSession(ctx, newID)
		}
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

func (a *Agent) forkJournal(ctx context.Context, source *sessionState, id, newID, title string) (bool, error) {
	if source == nil {
		return false, session.Fork(a.sessionsDir, id, newID, title)
	}
	var empty bool
	err := source.harness.RunExclusive(ctx, func(context.Context) error {
		// A new session has no journal until its first event, but can still
		// be forked with its model, effort, and permission settings intact.
		empty = len(source.harness.StateSnapshot().Events) == 0
		if empty {
			return nil
		}
		return session.Fork(a.sessionsDir, id, newID, title)
	})
	return empty, err
}

func (a *Agent) DeleteSession(ctx context.Context, id string) error {
	unlock, err := a.lockSession(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	a.mu.Lock()
	s, inMem := a.sessions[id]
	if inMem {
		delete(a.sessions, id)
	}
	a.mu.Unlock()

	var closeErr error
	if inMem {
		closeErr = s.close()
	}
	deleteErr := session.Delete(a.sessionsDir, id)
	if os.IsNotExist(deleteErr) {
		deleteErr = nil
	}
	return errors.Join(closeErr, deleteErr)
}

func (a *Agent) Save(id string) error {
	// Deletion removes the session before running end hooks. A save from one
	// of those hooks must stay a no-op rather than wait on its own deletion.
	if a.session(id) == nil {
		return nil
	}
	unlock, err := a.lockSession(context.Background(), id)
	if err != nil {
		return err
	}
	defer unlock()
	a.mu.Lock()
	s, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return nil
	}
	return session.Save(a.sessionsDir, id, s.harness.StateSnapshot())
}

func (a *Agent) SessionsDir() string { return a.sessionsDir }

func (a *Agent) Messages(id string) []harness.Message {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.harness.MessagesSnapshot()
}

func (a *Agent) HistorySnapshot(id string) code.HistorySnapshot {
	s := a.session(id)
	if s == nil {
		return code.HistorySnapshot{}
	}
	state := s.harness.StateSnapshot()
	return code.HistorySnapshot{Messages: state.Messages, Revision: state.Revision}
}

func (a *Agent) HistoryVersion(id string) code.HistoryVersion {
	s := a.session(id)
	if s == nil {
		return code.HistoryVersion{}
	}
	messageCount, revision := s.harness.StateVersion()
	return code.HistoryVersion{Revision: revision, MessageCount: messageCount}
}

func (a *Agent) Recap(ctx context.Context, id string) (string, error) {
	s := a.session(id)
	if s == nil {
		return "", fmt.Errorf("session %s not found", id)
	}
	return s.harness.Recap(ctx)
}

func (a *Agent) Compact(ctx context.Context, id, focus string) (harness.CompactResult, error) {
	s := a.session(id)
	if s == nil {
		return harness.CompactResult{}, fmt.Errorf("session %s not found", id)
	}
	return s.harness.Compact(hook.WithRuntime(ctx, s.hookRuntime()), focus)
}

func (a *Agent) ContextStats(id string) (harness.ContextStats, bool) {
	s := a.session(id)
	if s == nil {
		return harness.ContextStats{}, false
	}
	return s.harness.ContextStats(), true
}

func (a *Agent) Usage(id string) harness.Usage {
	s := a.session(id)
	if s == nil {
		return harness.Usage{}
	}
	return s.harness.UsageSnapshot()
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
	return s.resources.tasks
}

// ExecSessions exposes commands that exec left running so web clients
// can present them with the session's other background work.
func (a *Agent) ExecSessions(id string) []shell.ExecSessionInfo {
	s := a.session(id)
	if s == nil || s.resources.execManager == nil {
		return nil
	}
	return s.resources.execManager.BackgroundSessions()
}

func (a *Agent) ExecSession(id string, execID int) (shell.ExecSessionInfo, bool) {
	s := a.session(id)
	if s == nil || s.resources.execManager == nil {
		return shell.ExecSessionInfo{}, false
	}
	return s.resources.execManager.BackgroundSession(execID)
}

func (a *Agent) StopExecSession(id string, execID int) error {
	s := a.session(id)
	if s == nil || s.resources.execManager == nil {
		return fmt.Errorf("session not found")
	}
	return s.resources.execManager.StopBackgroundSession(execID)
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
	if err := s.harness.RecordMessage(msg); err != nil {
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
		running, _ := s.resources.tasks.Counts()
		total += running + len(s.resources.execManager.BackgroundSessions())
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
	if ok {
		a.lastActive = id
	}
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found; call NewSession first", id)
	}

	ctx = hook.WithRuntime(code.WithSessionID(ctx, id), s.hookRuntime())
	return s.harness.Send(ctx, input)
}

func (a *Agent) TurnFeatures(string) code.TurnFeatures {
	return code.TurnFeatures{Steer: true}
}

func (a *Agent) Steer(_ context.Context, id string, input code.TurnInput) error {
	s := a.session(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	if !s.harness.QueueInputWithID(input.Content, input.ID) {
		return code.ErrNoActiveTurn
	}
	return nil
}

func (a *Agent) Cancel(id string) {
	if s := a.session(id); s != nil {
		s.harness.Cancel(harness.ErrInterrupted)
	}
}

func (a *Agent) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := a.sessions
	a.sessions = nil
	a.lastActive = ""
	a.mu.Unlock()
	var errs []error
	for _, s := range sessions {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var wingmanModes = []code.Mode{
	{ID: code.AgentModeID, Name: "Agent", Description: "Make changes and run checks. Risky actions are reviewed automatically."},
	{ID: code.PlanModeID, Name: "Plan", Description: "Read-only. Proposes a plan."},
}

func (a *Agent) Modes(sessionID string) ([]code.Mode, string) {
	current := code.AgentModeID
	if s := a.session(sessionID); s != nil {
		current = string(s.currentMode())
	}
	return slices.Clone(wingmanModes), current
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
	return s.harness.AvailableTools()
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

func (a *Agent) promptContext(ctx context.Context) context.Context {
	if code.SessionIDFromContext(ctx) != "" {
		return ctx
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sid := hook.RuntimeFromContext(ctx).SessionID; a.sessions[sid] != nil {
		return code.WithSessionID(ctx, sid)
	}
	if a.cfg.IsolateSessions {
		return ctx
	}

	if a.sessions[a.lastActive] != nil {
		return code.WithSessionID(ctx, a.lastActive)
	}
	for sid := range a.sessions {
		return code.WithSessionID(ctx, sid)
	}
	return ctx
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

func (s *sessionState) prepareTurn(ctx context.Context, input []harness.Content) ([]harness.Content, error) {
	a := s.parent
	// Startup cannot ask a web client without session correlation. The harness
	// reserves the session and owns cancellation before entering preparation.
	if a.workspace.MCP != nil && len(a.workspace.MCP.TrustRequired) > 0 {
		if err := a.workspace.InitMCP(ctx, a.currentUI()); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			input = append(slices.Clone(input), harness.Content{Text: "Some MCP servers are unavailable: " + err.Error(), Hidden: true})
		}
	}
	return input, nil
}

func (s *sessionState) hookRuntime() hook.Runtime {
	return hook.Runtime{
		SessionID:      s.harness.CacheKey,
		CWD:            s.parent.workspace.RootPath,
		Model:          s.harness.Model(),
		PermissionMode: s.permissionMode(),
		StartSource:    s.startSource,
	}
}

func (s *sessionState) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.harness.Close(hook.WithRuntime(ctx, s.hookRuntime()), "other"); err != nil {
		return fmt.Errorf("close session %s: %w", s.id, err)
	}
	return nil
}

// watchFileChanges announces external file modifications mid-turn: while a
// turn runs, tracked files that changed outside the session's file tools are
// reported to the model as hidden context at the next model boundary. Idle
// periods are swept lazily by the UserPromptSubmit hook instead.
func (s *sessionState) watchFileChanges(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.harness.Running() {
				continue
			}
			s.sweepFileChanges()
			paths := s.takeFileChanges()
			if len(paths) == 0 {
				continue
			}
			notice := formatFileChangeNotice(paths)
			if !s.harness.QueueInput([]harness.Content{{Text: notice, Hidden: true}}) {
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

func (s *sessionState) catalogTools() []tool.Tool {
	mcpTools, lspTools := s.parent.workspace.ManagedTools()
	return slices.Concat(s.tools, mcpTools, lspTools)
}

func (s *sessionState) filterTools(tools []tool.Tool) []tool.Tool {
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
		ToolNames:      promptToolNames(s.harness.AvailableTools()),
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
	var shellContext string
	if s.scriptOptions != nil {
		shellContext = shell.ExecutionContext(ws.RootPath, s.scriptOptions.Shell)
	}
	return prompt.SectionData{
		ToolNames:           promptToolNames(s.harness.AvailableTools()),
		PlanMode:            s.currentMode() == modePlan,
		UnattendedMode:      s.unattended.Load(),
		Date:                now.Format("2006-01-02"),
		Timezone:            localTimezone(now),
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		WorkingDir:          ws.RootPath,
		Shell:               localShell(),
		FileReadRoots:       promptRoots(append([]string{ws.RootPath}, s.fileReadRoots...)),
		FileWriteRoots:      promptRoots(append([]string{ws.RootPath}, s.fileWriteRoots...)),
		ShellContext:        shellContext,
		MemoryDir:           ws.MemoryPath,
		MemoryContent:       ws.MemoryContent(),
		Skills:              skill.FormatForPrompt(ws.Skills()),
		ProjectInstructions: s.projectInstructions(),
	}
}

// promptRoots keeps existing roots once, omitting those inside an earlier listed root.
func promptRoots(roots []string) []string {
	var listed []string
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			continue
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		if slices.ContainsFunc(listed, func(parent string) bool {
			rel, err := filepath.Rel(parent, root)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}) {
			continue
		}
		listed = append(listed, root)
	}
	return listed
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
