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

	"github.com/google/uuid"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/external"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/ask"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/todo"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	skillpkg "github.com/adrianliechti/wingman-agent/pkg/skill"
)

type Agent struct {
	workspace *code.Workspace
	cfg       *harness.Config

	uiMu sync.RWMutex
	ui   code.UI

	sessionsDir string

	modelMu  sync.Mutex
	modelID  string
	effortID string
	upstream map[string]bool

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	parent *Agent
	aa     *harness.Agent

	modelID  string
	effortID string

	planMode    atomic.Bool
	baseTools   []tool.Tool
	execManager *shell.ExecManager

	projectInstructionsMu     sync.Mutex
	projectInstructionsCache  string
	projectInstructionsMtimes map[string]time.Time

	cancelMu  sync.Mutex
	cancelFn  context.CancelFunc
	cancelGen uint64
}

func New(ws *code.Workspace, cfg *harness.Config, ui code.UI) *Agent {
	return &Agent{
		workspace:   ws,
		cfg:         cfg,
		ui:          ui,
		modelID:     harness.DefaultModel(),
		sessionsDir: filepath.Join(filepath.Dir(ws.MemoryPath), "sessions"),
		sessions:    map[string]*sessionState{},
	}
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

func (a *Agent) Models(sessionID string) ([]code.Model, string) {
	return a.modelsFor(a.session(sessionID))
}

func (a *Agent) modelsFor(s *sessionState) ([]code.Model, string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()

	available := make([]code.Model, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		if a.upstream == nil || a.upstream[m.ID] {
			available = append(available, m)
		}
	}

	current := a.modelID
	if s != nil && s.modelID != "" {
		current = s.modelID
	}
	if len(available) > 0 && !slices.ContainsFunc(available, func(m code.Model) bool { return m.ID == current }) {
		current = available[0].ID
	}
	return available, current
}

func (a *Agent) SetModel(_ context.Context, sessionID, id string) error {
	s := a.session(sessionID)
	a.modelMu.Lock()
	a.modelID = id
	if s != nil {
		s.modelID = id
	}
	a.modelMu.Unlock()
	return nil
}

func (a *Agent) FetchModels(ctx context.Context) {
	models, err := a.cfg.Models(ctx)
	if err != nil {
		return
	}
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		ids[m.ID] = true
	}
	a.modelMu.Lock()
	a.upstream = ids
	a.modelMu.Unlock()
}

var effortValues = []string{"auto", "low", "medium", "high"}

func (a *Agent) Effort(sessionID string) (string, []string) {
	current := a.effortFor(a.session(sessionID))
	if current == "" {
		current = "auto"
	}
	return current, effortValues
}

func (a *Agent) effortFor(s *sessionState) string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if s != nil && s.effortID != "" {
		return s.effortID
	}
	return a.effortID
}

func (a *Agent) SetEffort(_ context.Context, sessionID, value string) error {
	switch value {
	case "", "auto":
		value = ""
	case "low", "medium", "high":
	default:
		return fmt.Errorf("effort must be auto, low, medium, or high (got %q)", value)
	}
	s := a.session(sessionID)
	a.modelMu.Lock()
	a.effortID = value
	if s != nil {
		s.effortID = value
	}
	a.modelMu.Unlock()
	return nil
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
	id := uuid.NewString()
	s := a.buildSession()
	s.aa.CacheKey = id
	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()
	return id, nil
}

func (a *Agent) LoadSession(_ context.Context, id string) error {
	a.mu.Lock()
	if _, ok := a.sessions[id]; ok {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	saved, err := session.Load(a.sessionsDir, id)
	if err != nil {
		return err
	}
	s := a.buildSession()
	s.aa.CacheKey = id
	s.aa.Messages = saved.State.Messages
	s.aa.Usage = saved.State.Usage

	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()
	return nil
}

func (a *Agent) DeleteSession(_ context.Context, id string) error {
	a.mu.Lock()
	s, inMem := a.sessions[id]
	if inMem {
		delete(a.sessions, id)
	}
	a.mu.Unlock()

	if inMem {
		s.cancel()
		s.execManager.Close()
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
	return session.Save(a.sessionsDir, id, harness.State{
		Messages: s.aa.Messages,
		Usage:    s.aa.Usage,
		Revision: s.aa.Revision,
	})
}

func (a *Agent) SessionsDir() string { return a.sessionsDir }

func (a *Agent) Messages(id string) []harness.Message {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.aa.Messages
}

func (a *Agent) Usage(id string) harness.Usage {
	s := a.session(id)
	if s == nil {
		return harness.Usage{}
	}
	return s.aa.Usage
}

func (a *Agent) session(id string) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *Agent) HasSession(id string) bool { return a.session(id) != nil }

func (a *Agent) Send(ctx context.Context, id string, input []harness.Content) iter.Seq2[harness.Message, error] {
	a.mu.Lock()
	s, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return errStream(fmt.Errorf("session %s not found; call NewSession first", id))
	}

	sendCtx, cancel := context.WithCancel(code.WithSessionID(ctx, id))
	stream := s.aa.Send(sendCtx, input)
	if stream == nil {
		cancel()
		return nil
	}

	gen := s.setCancel(cancel)

	return func(yield func(harness.Message, error) bool) {
		defer func() {
			s.clearCancel(gen)
			cancel()
		}()
		for msg, err := range stream {
			if !yield(msg, err) {
				return
			}
		}
	}
}

func (a *Agent) Cancel(id string) {
	if s := a.session(id); s != nil {
		s.cancel()
	}
}

func errStream(err error) iter.Seq2[harness.Message, error] {
	return func(yield func(harness.Message, error) bool) {
		yield(harness.Message{}, err)
	}
}

func (a *Agent) Close() error {
	a.mu.Lock()
	for _, s := range a.sessions {
		s.cancel()
		s.execManager.Close()
	}
	a.sessions = map[string]*sessionState{}
	a.mu.Unlock()
	return nil
}

var wingmanModes = []code.Mode{
	{ID: "agent", Name: "Agent", Description: "Read, edit, and run commands without asking."},
	{ID: "plan", Name: "Plan", Description: "Read-only — proposes a plan, doesn't edit code."},
}

func (a *Agent) Modes(sessionID string) ([]code.Mode, string) {
	out := make([]code.Mode, len(wingmanModes))
	copy(out, wingmanModes)
	current := "agent"
	if s := a.session(sessionID); s != nil && s.planMode.Load() {
		current = "plan"
	}
	return out, current
}

func (a *Agent) SetMode(_ context.Context, sessionID, modeID string) error {
	var plan bool
	switch modeID {
	case "agent":
		plan = false
	case "plan":
		plan = true
	default:
		return fmt.Errorf("unknown mode %q", modeID)
	}
	if s := a.session(sessionID); s != nil {
		s.planMode.Store(plan)
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

func (a *Agent) buildSession() *sessionState {
	sessionCfg := a.cfg.Derive()
	s := &sessionState{
		parent: a,
		aa:     &harness.Agent{Config: sessionCfg},
	}
	sessionCfg.Tools = s.tools
	sessionCfg.Instructions = s.instructions
	sessionCfg.Model = func() string {
		_, current := a.modelsFor(s)
		return current
	}
	sessionCfg.Effort = func() string {
		return a.effortFor(s)
	}
	elicit := a.buildElicit()
	ws := a.workspace

	globalHooks, globalErr := external.Load(userHooksConfigPath())
	workspaceHooksPath := filepath.Join(ws.RootPath, "hooks.json")
	workspaceHooks, workspaceErr := external.Load(workspaceHooksPath)

	// Fail closed: a hook config the user wrote but that no longer parses must
	// not silently disable the guards it configures.
	if err := errors.Join(globalErr, workspaceErr); err != nil {
		message := fmt.Sprintf("hook configuration is invalid; fix or remove it to unblock tools: %v", err)
		sessionCfg.Hooks.PreToolUse = append(sessionCfg.Hooks.PreToolUse,
			func(context.Context, tool.ToolCall) (string, error) { return message, nil },
		)
	}

	// Workspace hooks come with the repo, not the user — gate them behind a
	// one-time confirmation so a cloned project cannot run commands unprompted.
	var workspaceGate *external.Gate
	if rules := len(workspaceHooks.PreToolUse) + len(workspaceHooks.PostToolUse); rules > 0 {
		workspaceGate = &external.Gate{
			Confirm: elicit.Confirm,
			Message: fmt.Sprintf("Run %d workspace hook rule(s) from %s?", rules, workspaceHooksPath),
		}
	}

	sessionCfg.Hooks.PreToolUse = append(sessionCfg.Hooks.PreToolUse, globalHooks.PreHooks(ws.RootPath, nil)...)
	sessionCfg.Hooks.PreToolUse = append(sessionCfg.Hooks.PreToolUse, workspaceHooks.PreHooks(ws.RootPath, workspaceGate)...)
	sessionCfg.Hooks.PostToolUse = append(sessionCfg.Hooks.PostToolUse,
		truncation.New(ws.ScratchPath),
	)
	sessionCfg.Hooks.PostToolUse = append(sessionCfg.Hooks.PostToolUse, globalHooks.PostHooks(ws.RootPath, nil)...)
	sessionCfg.Hooks.PostToolUse = append(sessionCfg.Hooks.PostToolUse, workspaceHooks.PostHooks(ws.RootPath, workspaceGate)...)

	var allowedReadRoots []string
	for _, sk := range ws.Skills {
		if sk.Location != "" && filepath.IsAbs(sk.Location) {
			allowedReadRoots = append(allowedReadRoots, sk.Location)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		allowedReadRoots = append(allowedReadRoots, filepath.Join(home, ".wingman", "skills"))
	}
	allowedReadRoots = append(allowedReadRoots, ws.ScratchPath)

	var allowedWriteRoots []string
	if ws.MemoryPath != "" {
		allowedReadRoots = append(allowedReadRoots, ws.MemoryPath)
		allowedWriteRoots = append(allowedWriteRoots, ws.MemoryPath)
	}

	s.execManager = shell.NewExecManager()
	approvals := shell.NewApprovals()

	s.baseTools = slices.Concat(
		fs.Tools(ws.Root, &fs.Options{
			AllowedReadRoots:  allowedReadRoots,
			AllowedWriteRoots: allowedWriteRoots,
		}),
		shell.Tools(ws.RootPath, elicit, approvals),
		shell.ExecTools(s.execManager, ws.RootPath, elicit, approvals),
		todo.Tools(),
		ask.Tools(elicit),
		subagent.Tools(sessionCfg, s.subagentContext),
	)
	return s
}

func userHooksConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".wingman", "hooks.json")
}

func (a *Agent) buildElicit() *tool.Elicitation {
	return &tool.Elicitation{
		Ask: func(ctx context.Context, msg string) (string, error) {
			ui := a.currentUI()
			if ui == nil {
				return "", nil
			}
			return ui.Ask(ctx, msg)
		},
		Confirm: func(ctx context.Context, msg string) (bool, error) {
			ui := a.currentUI()
			if ui == nil {
				return true, nil
			}
			return ui.Confirm(ctx, msg)
		},
	}
}

func (s *sessionState) setCancel(fn context.CancelFunc) uint64 {
	s.cancelMu.Lock()
	prev := s.cancelFn
	s.cancelGen++
	gen := s.cancelGen
	s.cancelFn = fn
	s.cancelMu.Unlock()
	if prev != nil {
		prev()
	}
	return gen
}

func (s *sessionState) clearCancel(gen uint64) {
	s.cancelMu.Lock()
	if s.cancelGen == gen {
		s.cancelFn = nil
	}
	s.cancelMu.Unlock()
}

func (s *sessionState) cancel() {
	s.cancelMu.Lock()
	fn := s.cancelFn
	s.cancelMu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *sessionState) tools() []tool.Tool {
	tools := append([]tool.Tool{}, s.baseTools...)
	mcpTools, lspTools, graphTools := s.parent.workspace.ManagedTools()
	tools = append(tools, mcpTools...)
	tools = append(tools, lspTools...)
	tools = append(tools, graphTools...)
	if s.planMode.Load() {
		tools = planModeTools(tools)
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

func planModeEffectExecute(t tool.Tool) func(context.Context, map[string]any) (string, error) {
	return func(ctx context.Context, args map[string]any) (string, error) {
		if t.Effect == nil || t.Effect(args) != tool.EffectReadOnly {
			return "", fmt.Errorf("plan mode only allows read-only tool calls")
		}
		return t.Execute(ctx, args)
	}
}

func (s *sessionState) instructions() string {
	return BuildInstructions(s.instructionsData())
}

func (s *sessionState) subagentContext() string {
	return prompt.BuildAgentContext(s.instructionsData())
}

func BuildInstructions(data prompt.SectionData) string {
	base := prompt.Instructions
	if data.PlanMode {
		base = prompt.Planning
	}
	return prompt.BuildInstructions(base, data)
}

func (s *sessionState) instructionsData() prompt.SectionData {
	ws := s.parent.workspace
	return prompt.SectionData{
		PlanMode:            s.planMode.Load(),
		Date:                time.Now().Format("January 2, 2006"),
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		WorkingDir:          ws.RootPath,
		MemoryDir:           ws.MemoryPath,
		MemoryContent:       ws.MemoryContent(),
		Skills:              skillpkg.FormatForPrompt(ws.Skills),
		ProjectInstructions: s.projectInstructions(),
	}
}

const projectInstructionsMaxBytes = 25 * 1024

type projectInstructionsEntry struct {
	path  string
	rel   string
	mtime time.Time
}

func findProjectInstructions(wd string) []projectInstructionsEntry {
	wd = filepath.Clean(wd)
	var groups [][]projectInstructionsEntry
	for dir := wd; ; {
		var group []projectInstructionsEntry
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(dir, name)
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			rel, _ := filepath.Rel(wd, p)
			if rel == "" {
				rel = name
			}
			group = append(group, projectInstructionsEntry{path: p, rel: rel, mtime: info.ModTime()})
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Root-level guidance first, most-specific (closest to wd) last, so the
	// deeper file reads as overriding the general one.
	var found []projectInstructionsEntry
	for i := len(groups) - 1; i >= 0; i-- {
		found = append(found, groups[i]...)
	}
	return found
}

func renderProjectInstructions(entries []projectInstructionsEntry) (string, map[string]time.Time) {
	parts := make([]string, 0, len(entries))
	mtimes := make(map[string]time.Time, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		data, err := os.ReadFile(e.path)
		if err != nil {
			continue
		}
		mtimes[e.path] = e.mtime
		content := strings.TrimSpace(string(data))
		if content == "" || seen[content] {
			continue
		}
		seen[content] = true
		parts = append(parts, fmt.Sprintf("From %s:\n\n%s", e.rel, content))
	}
	result := strings.Join(parts, "\n\n---\n\n")
	if len(result) > projectInstructionsMaxBytes {
		result = result[:projectInstructionsMaxBytes] + "\n\n[truncated]"
	}
	return result, mtimes
}

func (s *sessionState) projectInstructions() string {
	s.projectInstructionsMu.Lock()
	defer s.projectInstructionsMu.Unlock()

	found := findProjectInstructions(s.parent.workspace.RootPath)
	if len(found) == len(s.projectInstructionsMtimes) {
		unchanged := true
		for _, e := range found {
			if prev, ok := s.projectInstructionsMtimes[e.path]; !ok || !prev.Equal(e.mtime) {
				unchanged = false
				break
			}
		}
		if unchanged {
			return s.projectInstructionsCache
		}
	}
	result, mtimes := renderProjectInstructions(found)
	s.projectInstructionsCache = result
	s.projectInstructionsMtimes = mtimes
	return result
}

func ReadProjectInstructions(wd string) string {
	result, _ := renderProjectInstructions(findProjectInstructions(wd))
	return result
}
