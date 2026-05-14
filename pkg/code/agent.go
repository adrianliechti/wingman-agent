package code

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/ask"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fetch"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/search"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
)

// Agent is a single conversation against a Workspace. Holds Messages and
// Usage (via the embedded *agent.Agent) plus the per-session flags.
// *Workspace is embedded anonymously so callers can reach workdir state
// directly — agent.RootPath, agent.LSP, agent.WarmUp() — without a dot
// chain.
type Agent struct {
	*agent.Agent
	*Workspace

	PlanMode bool

	// baseTools is the session-specific tool list (fs/shell/fetch/search/
	// ask/subagent). Stored on Agent because shell.Tools and ask.Tools
	// close over the session's UI elicit. MCP/LSP tools live on Workspace
	// and get merged in tools() at call time.
	baseTools []tool.Tool

	lastMemoryHash string
	mu             sync.Mutex
}

// NewAgent constructs an Agent bound to this Workspace, deriving its
// Config from the template `cfg` (which carries the upstream API client +
// late-bound model/effort getters). The server uses this to spin up many
// agents (one per session) sharing one Workspace + one Config. The TUI/
// Wails app call it as: ws := code.NewWorkspace(wd); a := ws.NewAgent(cfg, nil).
func (ws *Workspace) NewAgent(cfg *agent.Config, ui UI) *Agent {
	sessionCfg := cfg.Derive()

	a := &Agent{
		Agent:     &agent.Agent{Config: sessionCfg},
		Workspace: ws,
	}

	sessionCfg.Tools = a.tools
	sessionCfg.ContextMessages = a.memoryContextMessages
	sessionCfg.Instructions = a.Instructions

	// Truncation hook: cap large tool outputs and dump the full text to the
	// workspace's scratch dir so the model can `read` a specific range if
	// it needs the elided middle. Every caller (TUI, server) wants this; do
	// it once here instead of forcing each to remember.
	sessionCfg.Hooks.PostToolUse = append(sessionCfg.Hooks.PostToolUse,
		truncation.New(truncation.DefaultMaxBytes, ws.ScratchPath),
	)

	// Build the session-specific base tool list. Shell + ask depend on the
	// elicit closure (per-session UI prompts); subagent embeds sessionCfg
	// so child agents inherit this session's model/effort.
	elicit := buildElicit(ui)

	var allowedReadRoots []string
	for _, s := range ws.Skills {
		if s.Location != "" && filepath.IsAbs(s.Location) {
			allowedReadRoots = append(allowedReadRoots, s.Location)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		allowedReadRoots = append(allowedReadRoots, filepath.Join(home, ".wingman", "skills"))
	}
	allowedReadRoots = append(allowedReadRoots, ws.ScratchPath)

	a.baseTools = slices.Concat(
		fs.Tools(ws.Root, allowedReadRoots...),
		shell.Tools(ws.RootPath, elicit),
		fetch.Tools(),
		search.Tools(),
		ask.Tools(elicit),
		subagent.Tools(sessionCfg),
	)

	return a
}

func buildElicit(ui UI) *tool.Elicitation {
	return &tool.Elicitation{
		Ask: func(ctx context.Context, msg string) (string, error) {
			if ui == nil {
				return "", nil
			}
			return ui.Ask(ctx, msg)
		},
		Confirm: func(ctx context.Context, msg string) (bool, error) {
			if ui == nil {
				return true, nil
			}
			return ui.Confirm(ctx, msg)
		},
	}
}

// tools returns the merged tool list for this turn: session base +
// workspace MCP + workspace LSP, filtered through plan mode if active.
func (a *Agent) tools() []tool.Tool {
	tools := append([]tool.Tool{}, a.baseTools...)

	mcpTools, lspTools := a.managedTools()
	tools = append(tools, mcpTools...)
	tools = append(tools, lspTools...)

	if a.PlanMode {
		tools = planModeTools(tools)
	}

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

// Memory and plan content

const (
	memoryFileName      = "MEMORY.md"
	memoryMaxBytes      = 25 * 1024
	memoryContextPrefix = "Current MEMORY.md:\n\n"
	memoryContextEmpty  = "MEMORY.md is currently empty."
)

func (a *Agent) memoryContextMessages() []agent.Message {
	content := a.MemoryContent()
	messageText := ""
	if content != "" {
		messageText = memoryContextPrefix + content
	}

	sum := sha256.Sum256([]byte(content))
	hash := string(sum[:])

	a.mu.Lock()
	defer a.mu.Unlock()

	prevHash := a.lastMemoryHash
	if hash == a.lastMemoryHash {
		return nil
	}
	a.lastMemoryHash = hash

	if messageText == "" {
		messageText = memoryContextEmpty
		if prevHash == "" && a.latestMemoryContextText() == "" {
			return nil
		}
	}

	if prevHash == "" && a.latestMemoryContextText() == messageText {
		return nil
	}

	return []agent.Message{{
		Role:   agent.RoleUser,
		Hidden: true,
		Content: []agent.Content{{
			Text: messageText,
		}},
	}}
}

func (a *Agent) latestMemoryContextText() string {
	if a.Agent == nil {
		return ""
	}

	for i := len(a.Messages) - 1; i >= 0; i-- {
		m := a.Messages[i]
		if !m.Hidden || m.Role != agent.RoleUser || len(m.Content) != 1 {
			continue
		}

		text := m.Content[0].Text
		if strings.HasPrefix(text, memoryContextPrefix) || text == memoryContextEmpty {
			return text
		}
	}

	return ""
}

// Instructions

// Instructions renders the full system prompt for this turn (plan vs agent
// base + current SectionData). Wired into Config.Instructions in NewAgent
// so the agent.Send loop reads the live value each turn.
func (a *Agent) Instructions() string {
	return BuildInstructions(a.InstructionsData())
}

func BuildInstructions(data prompt.SectionData) string {
	base := prompt.Instructions

	if data.PlanMode {
		base = prompt.Planning
	}

	return prompt.BuildInstructions(base, data)
}

func (a *Agent) InstructionsData() prompt.SectionData {
	data := prompt.SectionData{
		PlanMode:            a.PlanMode,
		Date:                time.Now().Format("January 2, 2006"),
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		WorkingDir:          a.RootPath,
		MemoryDir:           a.MemoryPath,
		Skills:              skill.FormatForPrompt(a.Skills),
		ProjectInstructions: ReadProjectInstructions(a.RootPath),
	}

	return data
}

const projectInstructionsMaxBytes = 25 * 1024

// ReadProjectInstructions walks from wd up to the filesystem root,
// collecting AGENTS.md and CLAUDE.md files. Returns them concatenated
// with headers, closest ancestor first. Truncates at 25KB.
func ReadProjectInstructions(wd string) string {
	var parts []string

	dir := filepath.Clean(wd)

	for {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}

			content := strings.TrimSpace(string(data))
			if content == "" {
				continue
			}

			rel, _ := filepath.Rel(wd, filepath.Join(dir, name))
			if rel == "" {
				rel = name
			}

			parts = append(parts, fmt.Sprintf("From %s:\n\n%s", rel, content))
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}

		dir = parent
	}

	result := strings.Join(parts, "\n\n---\n\n")

	if len(result) > projectInstructionsMaxBytes {
		result = result[:projectInstructionsMaxBytes] + "\n\n[truncated]"
	}

	return result
}
