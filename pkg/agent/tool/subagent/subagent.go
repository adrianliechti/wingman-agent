package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const instructions = "You are an agent performing a specific delegated task. Complete only the assigned scope. Unless the task explicitly asks you to edit files, stay read-only. When done, provide a concise result with file:line references when relevant, followed by any uncertainty or verification gaps. Do not explain your process."

func Tools(cfg *agent.Config) []tool.Tool {
	description := strings.Join([]string{
		"Launch a sub-agent for a bounded delegated task. Only the final answer is returned — intermediate tool calls stay out of your context.",
		"- Use for codebase-wide research, find-all-usages, or independent subtasks. Skip for 1-2 tool-call work.",
		"- The agent has no access to your conversation. Write a self-contained prompt: file paths, symbols, constraints, expected output shape.",
		"- State explicitly whether the agent may edit files or is read-only. Do not delegate synthesis — integrate findings yourself.",
	}, "\n")

	return []tool.Tool{{
		Name:        "agent",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectMutates),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "Self-contained task briefing."},
			},

			"required": []string{"prompt"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			prompt, ok := args["prompt"].(string)

			if !ok || prompt == "" {
				return "", fmt.Errorf("prompt is required")
			}

			subcfg := cfg.Derive()
			subcfg.Instructions = func() string { return instructions }
			subcfg.Tools = func() []tool.Tool {
				if cfg.Tools == nil {
					return nil
				}

				var filtered []tool.Tool

				for _, t := range cfg.Tools() {
					if t.Name == "agent" || t.Hidden {
						continue
					}

					filtered = append(filtered, t)
				}

				return filtered
			}

			sub := &agent.Agent{Config: subcfg}

			var result strings.Builder

			for msg, err := range sub.Send(ctx, []agent.Content{{Text: prompt}}) {
				if err != nil {
					return "", fmt.Errorf("agent error: %w", err)
				}

				for _, c := range msg.Content {
					if c.Text != "" {
						result.WriteString(c.Text)
					}
				}
			}

			text := strings.TrimSpace(result.String())

			if text == "" {
				return "Sub-agent completed but produced no output.", nil
			}

			return text, nil
		},
	}}
}
