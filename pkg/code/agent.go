package code

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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

type Agent struct {
	*agent.Agent
	*Workspace

	PlanMode bool

	// baseTools closes over the session's UI elicit (shell, ask) and
	// sessionCfg (subagent); MCP/LSP tools live on Workspace and merge in
	// tools() at call time.
	baseTools []tool.Tool

	projectInstructionsMu     sync.Mutex
	projectInstructionsCache  string
	projectInstructionsMtimes map[string]time.Time
}

func (ws *Workspace) NewAgent(cfg *agent.Config, ui UI) *Agent {
	sessionCfg := cfg.Derive()

	a := &Agent{
		Agent:     &agent.Agent{Config: sessionCfg},
		Workspace: ws,
	}

	sessionCfg.Tools = a.tools
	sessionCfg.Instructions = a.Instructions

	sessionCfg.Hooks.PostToolUse = append(sessionCfg.Hooks.PostToolUse,
		truncation.New(ws.ScratchPath),
	)

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

func (a *Agent) tools() []tool.Tool {
	tools := append([]tool.Tool{}, a.baseTools...)

	mcpTools, lspTools := a.managedTools()
	tools = append(tools, mcpTools...)
	tools = append(tools, lspTools...)

	if a.PlanMode {
		tools = planModeTools(tools)
	}

	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
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

const (
	memoryFileName = "MEMORY.md"
	memoryMaxBytes = 25 * 1024
)

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
		MemoryContent:       a.MemoryContent(),
		Skills:              skill.FormatForPrompt(a.Skills),
		ProjectInstructions: a.projectInstructions(),
	}

	return data
}

const projectInstructionsMaxBytes = 25 * 1024

// projectInstructions returns the rendered AGENTS.md / CLAUDE.md block.
// It walks ancestor dirs but only re-reads when an mtime changed; otherwise
// returns the cached string so the static prefix stays byte-stable across turns.
func (a *Agent) projectInstructions() string {
	a.projectInstructionsMu.Lock()
	defer a.projectInstructionsMu.Unlock()

	type entry struct {
		path  string
		rel   string
		mtime time.Time
	}

	wd := filepath.Clean(a.RootPath)
	var found []entry

	dir := wd
	for {
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
			found = append(found, entry{path: p, rel: rel, mtime: info.ModTime()})
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if len(found) == len(a.projectInstructionsMtimes) {
		unchanged := true
		for _, e := range found {
			if prev, ok := a.projectInstructionsMtimes[e.path]; !ok || !prev.Equal(e.mtime) {
				unchanged = false
				break
			}
		}
		if unchanged {
			return a.projectInstructionsCache
		}
	}

	var parts []string
	mtimes := make(map[string]time.Time, len(found))
	for _, e := range found {
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

	a.projectInstructionsCache = result
	a.projectInstructionsMtimes = mtimes
	return result
}

// ReadProjectInstructions walks from wd up to the filesystem root,
// concatenating AGENTS.md / CLAUDE.md with headers, closest ancestor first.
// Truncates at 25KB. Use Agent.projectInstructions for the cached path.
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
