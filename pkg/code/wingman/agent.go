// Package wingman is the in-process [code.Agent] implementation. One
// Agent owns the workspace and all open sessions for that workspace;
// each session has its own [*agent.Agent], transcript, and usage. The
// shared workspace surfaces MCP/LSP/Rewind through code.Workspace.
package wingman

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/ask"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/webfetch"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/websearch"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	skillpkg "github.com/adrianliechti/wingman-agent/pkg/skill"
)

// Agent is the wingman in-process implementation of [code.Agent]. Model
// and effort are agent-wide (mirrors the previous server.s.model/s.effort
// design) and applied to every session's request via closures on the
// embedded agent.Config.
type Agent struct {
	workspace *code.Workspace
	cfg       *agent.Config
	ui        code.UI

	sessionsDir string

	modelMu  sync.Mutex
	modelID  string
	effortID string

	upstreamMu sync.Mutex
	upstream   map[string]bool

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	parent *Agent
	aa     *agent.Agent

	planMode  bool
	baseTools []tool.Tool

	projectInstructionsMu     sync.Mutex
	projectInstructionsCache  string
	projectInstructionsMtimes map[string]time.Time

	cancelMu sync.Mutex
	cancelFn context.CancelFunc
}

// New constructs a wingman.Agent rooted at the workspace.
func New(ws *code.Workspace, cfg *agent.Config, ui code.UI) *Agent {
	return &Agent{
		workspace:   ws,
		cfg:         cfg,
		ui:          ui,
		sessionsDir: filepath.Join(filepath.Dir(ws.MemoryPath), "sessions"),
		sessions:    map[string]*sessionState{},
	}
}

func (a *Agent) Name() string               { return code.BuiltinAgentName }
func (a *Agent) Workspace() *code.Workspace { return a.workspace }

// ─── Models ──────────────────────────────────────────────────────

// FetchUpstreamModels populates the cache of upstream model ids so
// subsequent Models() calls can filter [code.AvailableModels] down to
// what the configured upstream actually serves. Call once at startup.
func (a *Agent) FetchUpstreamModels(ctx context.Context) {
	models, err := a.cfg.Models(ctx)
	if err != nil {
		return
	}
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		ids[m.ID] = true
	}
	a.upstreamMu.Lock()
	a.upstream = ids
	a.upstreamMu.Unlock()
}

func (a *Agent) Models() ([]code.Model, string) {
	a.upstreamMu.Lock()
	upstream := a.upstream
	a.upstreamMu.Unlock()

	a.modelMu.Lock()
	current := a.modelID
	a.modelMu.Unlock()

	if upstream == nil {
		out := make([]code.Model, len(code.AvailableModels))
		copy(out, code.AvailableModels)
		return out, current
	}
	out := make([]code.Model, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		if upstream[m.ID] {
			out = append(out, m)
		}
	}
	return out, current
}

func (a *Agent) SetModel(_ context.Context, id string) error {
	a.modelMu.Lock()
	a.modelID = id
	a.modelMu.Unlock()
	return nil
}

func (a *Agent) currentModel() string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.modelID
}

// AutoSelectModel picks a default model id from the upstream catalog
// when none has been set yet.
func (a *Agent) AutoSelectModel(ctx context.Context) {
	if a.currentModel() != "" {
		return
	}
	a.FetchUpstreamModels(ctx)
	a.upstreamMu.Lock()
	upstream := a.upstream
	a.upstreamMu.Unlock()
	for _, m := range code.AvailableModels {
		if upstream == nil || upstream[m.ID] {
			_ = a.SetModel(ctx, m.ID)
			return
		}
	}
}

// ─── Effort ──────────────────────────────────────────────────────

var effortValues = []string{"auto", "low", "medium", "high"}

func (a *Agent) Effort() (string, []string) {
	a.modelMu.Lock()
	current := a.effortID
	a.modelMu.Unlock()
	if current == "" {
		current = "auto"
	}
	return current, effortValues
}

func (a *Agent) SetEffort(_ context.Context, value string) error {
	switch value {
	case "", "auto":
		a.modelMu.Lock()
		a.effortID = ""
		a.modelMu.Unlock()
	case "low", "medium", "high":
		a.modelMu.Lock()
		a.effortID = value
		a.modelMu.Unlock()
	default:
		return fmt.Errorf("effort must be auto, low, medium, or high (got %q)", value)
	}
	return nil
}

func (a *Agent) currentEffort() string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.effortID
}

// ─── Sessions ────────────────────────────────────────────────────

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

// NewSession mints a UUID and lazy-builds the in-memory state. Nothing
// is persisted to disk yet — that happens after the first Save().
func (a *Agent) NewSession(_ context.Context) (string, error) {
	id := uuid.NewString()
	a.mu.Lock()
	a.sessions[id] = a.buildSession()
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
	}
	if err := session.Delete(a.sessionsDir, id); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Save persists a session's transcript. Called after each turn by
// consumers that want chat to survive a restart. No-op if not loaded.
func (a *Agent) Save(id string) error {
	a.mu.Lock()
	s, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok {
		return nil
	}
	return session.Save(a.sessionsDir, id, agent.State{
		Messages: s.aa.Messages,
		Usage:    s.aa.Usage,
	})
}

func (a *Agent) SessionsDir() string { return a.sessionsDir }

func (a *Agent) Messages(id string) []agent.Message {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.aa.Messages
}

func (a *Agent) Usage(id string) agent.Usage {
	s := a.session(id)
	if s == nil {
		return agent.Usage{}
	}
	return s.aa.Usage
}

func (a *Agent) session(id string) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

// HasSession reports whether the agent is tracking a session id in
// memory. Useful for the server's lazy-create check (don't restore
// state for a session that's already in memory).
func (a *Agent) HasSession(id string) bool { return a.session(id) != nil }

// ─── Send / Cancel ───────────────────────────────────────────────

func (a *Agent) Send(ctx context.Context, id string, input []agent.Content) iter.Seq2[agent.Message, error] {
	a.mu.Lock()
	s, ok := a.sessions[id]
	if !ok {
		// Lazy-create — Send to an unknown id materializes the session.
		s = a.buildSession()
		a.sessions[id] = s
	}
	a.mu.Unlock()

	sendCtx, cancel := context.WithCancel(ctx)
	s.setCancel(cancel)
	stream := s.aa.Send(sendCtx, input)
	if stream == nil {
		cancel()
		s.clearCancel()
		return nil
	}

	return func(yield func(agent.Message, error) bool) {
		defer func() {
			s.clearCancel()
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

// ─── Lifecycle ───────────────────────────────────────────────────

func (a *Agent) Close() error {
	a.mu.Lock()
	for _, s := range a.sessions {
		s.cancel()
	}
	a.sessions = map[string]*sessionState{}
	a.mu.Unlock()
	return nil
}

// ─── Plan mode (wingman-only affordance) ────────────────────────

func (a *Agent) SetPlanMode(id string, plan bool) {
	if s := a.session(id); s != nil {
		s.planMode = plan
	}
}

func (a *Agent) PlanMode(id string) bool {
	s := a.session(id)
	return s != nil && s.planMode
}

// Tools returns the snapshot tool list for a session (used by the TUI
// to enumerate tool names and check the Hidden flag for the help/picker
// menus). Returns nil for unknown ids.
func (a *Agent) Tools(id string) []tool.Tool {
	s := a.session(id)
	if s == nil {
		return nil
	}
	return s.tools()
}

// ─── session-state plumbing ──────────────────────────────────────

func (a *Agent) buildSession() *sessionState {
	sessionCfg := a.cfg.Derive()
	s := &sessionState{
		parent: a,
		aa:     &agent.Agent{Config: sessionCfg},
	}
	sessionCfg.Tools = s.tools
	sessionCfg.Instructions = s.instructions
	sessionCfg.Model = a.currentModel
	sessionCfg.Effort = a.currentEffort
	sessionCfg.Hooks.PostToolUse = append(sessionCfg.Hooks.PostToolUse,
		truncation.New(a.workspace.ScratchPath),
	)

	elicit := buildElicit(a.ui)
	ws := a.workspace

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

	s.baseTools = slices.Concat(
		fs.Tools(ws.Root, &fs.Options{
			AllowedReadRoots:  allowedReadRoots,
			AllowedWriteRoots: allowedWriteRoots,
		}),
		shell.Tools(ws.RootPath, elicit),
		webfetch.Tools(),
		websearch.Tools(),
		ask.Tools(elicit),
		subagent.Tools(sessionCfg),
	)
	return s
}

func buildElicit(ui code.UI) *tool.Elicitation {
	if ui == nil {
		return nil
	}
	return &tool.Elicitation{
		Ask: func(ctx context.Context, msg string) (string, error) {
			return ui.Ask(ctx, msg)
		},
		Confirm: func(ctx context.Context, msg string) (bool, error) {
			return ui.Confirm(ctx, msg)
		},
	}
}

func (s *sessionState) setCancel(fn context.CancelFunc) {
	s.cancelMu.Lock()
	prev := s.cancelFn
	s.cancelFn = fn
	s.cancelMu.Unlock()
	if prev != nil {
		prev()
	}
}

func (s *sessionState) clearCancel() {
	s.cancelMu.Lock()
	s.cancelFn = nil
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
	mcpTools, lspTools := s.parent.workspace.ManagedTools()
	tools = append(tools, mcpTools...)
	tools = append(tools, lspTools...)
	if s.planMode {
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

// ─── Instructions / project context ─────────────────────────────

func (s *sessionState) instructions() string {
	return BuildInstructions(s.instructionsData())
}

// BuildInstructions composes the system prompt from a section-data
// snapshot.
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
		PlanMode:            s.planMode,
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
	var found []projectInstructionsEntry
	for dir := wd; ; {
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
			found = append(found, projectInstructionsEntry{path: p, rel: rel, mtime: info.ModTime()})
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return found
}

func renderProjectInstructions(entries []projectInstructionsEntry) (string, map[string]time.Time) {
	parts := make([]string, 0, len(entries))
	mtimes := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		data, err := os.ReadFile(e.path)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("From %s:\n\n%s", e.rel, content))
		mtimes[e.path] = e.mtime
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

// ReadProjectInstructions is an uncached read for callers (TUI status
// bar, /init flows) that need the assembled block without a session.
func ReadProjectInstructions(wd string) string {
	result, _ := renderProjectInstructions(findProjectInstructions(wd))
	return result
}
