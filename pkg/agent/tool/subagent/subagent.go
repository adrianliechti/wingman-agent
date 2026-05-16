package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type subagentType struct {
	Name         string
	Description  string
	Instructions string
	AllowTool    func(tool.Tool) bool
}

const generalPurposeInstructions = `You are an agent performing a specific delegated task. Complete only the assigned scope. Unless the task explicitly asks you to edit files, stay read-only. Search broadly when you do not know where something lives, then narrow down. Prefer editing existing files over creating new files, and never create documentation files unless explicitly requested. Lead with findings or changes made, grouped by file when relevant, using file:line references. State assumptions and verification gaps at the end, not at the start. Reply in <=200 words unless the task explicitly asks for more. Do not explain your process.`

const exploreInstructions = `You are a read-only codebase exploration specialist. Complete the caller's search or analysis request efficiently. You may search, read, inspect git state, fetch URLs, and use read-only LSP or shell commands. You must not create, modify, delete, move, or copy files, install dependencies, or run mutating git commands. Use grep/find/LSP before broad reads, read only the line windows that matter, and use parallel tool calls when searches or reads are independent. Lead with the answer and cite file:line references. End with assumptions or gaps only if they matter. Reply in <=200 words unless the task explicitly asks for more.`

const verificationInstructions = `You are a verification specialist. Your job is to test whether the implementation actually works, not to confirm by reading code. You must not modify project files, install dependencies, or run git write operations. You may run read-only inspection commands and normal build, test, lint, type-check, or local execution commands. For UI, API, CLI, migration, and integration changes, exercise the behavior directly when possible. Include the exact commands you ran, the relevant output, and a verdict. End with exactly one line: VERDICT: PASS, VERDICT: FAIL, or VERDICT: PARTIAL.`

var subagentTypes = map[string]subagentType{
	"general-purpose": {
		Name:         "general-purpose",
		Description:  "General-purpose agent for complex searches, multi-step research, and scoped implementation work.",
		Instructions: generalPurposeInstructions,
		AllowTool:    allowNonAgentTool,
	},
	"explore": {
		Name:         "explore",
		Description:  "Read-only codebase exploration agent for finding files, tracing usage, and answering architecture questions.",
		Instructions: exploreInstructions,
		AllowTool:    allowReadOnlyTool,
	},
	"verification": {
		Name:         "verification",
		Description:  "Verification-only agent for running checks and trying to break an implementation before reporting completion.",
		Instructions: verificationInstructions,
		AllowTool:    allowVerificationTool,
	},
}

func Tools(cfg *agent.Config) []tool.Tool {
	availableTypes := []string{"general-purpose", "explore", "verification"}

	description := strings.Join([]string{
		"Launch a sub-agent for a bounded delegated task. Only the final answer is returned; intermediate tool output stays out of your context.",
		"- Use for open-ended searches, codebase-wide research, independent verification, or scoped implementation work that can proceed without blocking your next local step.",
		"- Skip for directed 1-2 tool-call work: use `read`, `find`, `grep`, or LSP directly when you already know the path, symbol, or exact query.",
		"- Choose `subagent_type`: `explore` for read-only codebase research, `verification` for checking an implementation, `general-purpose` for multi-step research or implementation. Defaults to `general-purpose`.",
		"- Fresh sub-agents start with zero conversation context. Brief them like a capable colleague who just joined: goal, relevant paths/symbols, what you already learned or ruled out, allowed edit scope, and expected output shape. Keep prompts concise but complete; ask for a short report when enough.",
		"- Never delegate synthesis. Use the returned report as input, then make the decision or user-facing summary yourself.",
	}, "\n")

	return []tool.Tool{{
		Name:        "agent",
		Description: description,
		Effect:      classifyEffect,

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"description": map[string]any{"type": "string", "description": "Short 3-5 word label for the UI (for example, `Audit auth middleware`)."},
				"prompt":      map[string]any{"type": "string", "description": "Self-contained task briefing. Include goal, relevant paths/symbols, allowed edit scope, what is out of scope, and the expected report shape."},
				"subagent_type": map[string]any{
					"type":        "string",
					"description": "Agent type to use. `explore` is read-only; `verification` runs checks without editing; `general-purpose` can research or implement within the prompt scope.",
					"enum":        availableTypes,
				},
				"model":  map[string]any{"type": "string", "description": "Optional model override for this sub-agent. Omit to inherit the parent model."},
				"effort": map[string]any{"type": "string", "description": "Optional reasoning effort override for this sub-agent. Omit to inherit the parent effort.", "enum": []string{"low", "medium", "high"}},
			},

			"required": []string{"description", "prompt"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			description, ok := args["description"].(string)

			if !ok || strings.TrimSpace(description) == "" {
				return "", fmt.Errorf("description is required")
			}

			prompt, ok := args["prompt"].(string)

			if !ok || strings.TrimSpace(prompt) == "" {
				return "", fmt.Errorf("prompt is required")
			}

			subagentName := "general-purpose"
			if raw, ok := args["subagent_type"].(string); ok && strings.TrimSpace(raw) != "" {
				subagentName = strings.ToLower(strings.TrimSpace(raw))
			}

			typ, ok := subagentTypes[subagentName]
			if !ok {
				return "", fmt.Errorf("unknown subagent_type %q (available: %s)", subagentName, strings.Join(availableTypes, ", "))
			}

			subcfg := cfg.Derive()
			subcfg.Instructions = func() string { return typ.Instructions }

			if model, ok := args["model"].(string); ok && strings.TrimSpace(model) != "" {
				v := strings.TrimSpace(model)
				subcfg.Model = func() string { return v }
			}

			if effort, ok := args["effort"].(string); ok && strings.TrimSpace(effort) != "" {
				v := strings.ToLower(strings.TrimSpace(effort))
				if !validEffort(v) {
					return "", fmt.Errorf("effort must be low, medium, or high")
				}
				subcfg.Effort = func() string { return v }
			}

			subcfg.Tools = func() []tool.Tool {
				if cfg.Tools == nil {
					return nil
				}

				return toolsForType(cfg.Tools(), typ)
			}

			sub := &agent.Agent{Config: subcfg}

			// Only the final assistant text is the "answer". Reset the buffer
			// on each new assistant text message; the last message wins. This
			// drops intermediate narration like "I'll start by exploring…"
			// that the model emits before tool calls.
			var lastText strings.Builder

			for msg, err := range sub.Send(ctx, []agent.Content{{Text: prompt}}) {
				if err != nil {
					return "", fmt.Errorf("agent error: %w", err)
				}

				var msgText strings.Builder
				for _, c := range msg.Content {
					if c.Text != "" {
						msgText.WriteString(c.Text)
					}
				}

				if msgText.Len() > 0 {
					lastText.Reset()
					lastText.WriteString(msgText.String())
				}
			}

			text := strings.TrimSpace(lastText.String())

			if text == "" {
				return "Sub-agent completed but produced no output.", nil
			}

			return text, nil
		},
	}}
}

func classifyEffect(args map[string]any) tool.Effect {
	if args == nil {
		return tool.EffectDynamic
	}

	subagentName := "general-purpose"
	if raw, ok := args["subagent_type"].(string); ok && strings.TrimSpace(raw) != "" {
		subagentName = strings.ToLower(strings.TrimSpace(raw))
	}

	if subagentName == "explore" {
		return tool.EffectReadOnly
	}

	return tool.EffectMutates
}

func toolsForType(tools []tool.Tool, typ subagentType) []tool.Tool {
	filtered := make([]tool.Tool, 0, len(tools))

	for _, t := range tools {
		if !typ.AllowTool(t) {
			continue
		}

		if typ.Name == "explore" && t.Effect != nil && t.Effect(nil) == tool.EffectDynamic {
			t = readOnlyDynamicTool(t)
		}

		filtered = append(filtered, t)
	}

	return filtered
}

func readOnlyDynamicTool(t tool.Tool) tool.Tool {
	originalExecute := t.Execute
	originalEffect := t.Effect

	t.Execute = func(ctx context.Context, args map[string]any) (string, error) {
		if originalEffect(args) != tool.EffectReadOnly {
			return "", fmt.Errorf("explore subagent only allows read-only %s calls", t.Name)
		}
		if originalExecute == nil {
			return "", fmt.Errorf("tool %s has no executor", t.Name)
		}
		return originalExecute(ctx, args)
	}

	return t
}

func allowNonAgentTool(t tool.Tool) bool {
	return t.Name != "agent" && !t.Hidden
}

func allowReadOnlyTool(t tool.Tool) bool {
	if t.Name == "ask_user" {
		return false
	}

	if !allowNonAgentTool(t) || t.Effect == nil {
		return false
	}

	switch t.Effect(nil) {
	case tool.EffectReadOnly:
		return true
	case tool.EffectDynamic:
		return true
	default:
		return false
	}
}

func allowVerificationTool(t tool.Tool) bool {
	if !allowNonAgentTool(t) {
		return false
	}

	switch t.Name {
	case "write", "edit", "todo_write", "ask_user":
		return false
	default:
		return true
	}
}

func validEffort(effort string) bool {
	switch effort {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}
