package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const instructions = "You are an agent performing a specific delegated task. Complete only the assigned scope. Unless the task explicitly asks you to edit files, stay read-only. Lead with findings, grouped by file when relevant, using `file:line` references. State assumptions and verification gaps at the end, not at the start. Reply in ≤200 words unless the task explicitly asks for more. Do not explain your process."

func Tools(cfg *agent.Config) []tool.Tool {
	description := strings.Join([]string{
		"Launch a sub-agent for a bounded delegated task. Only the sub-agent's final answer is returned — intermediate tool calls stay out of your context.",
		"- Use for codebase-wide research, find-all-usages, or independent subtasks. Skip for 1-2 tool-call work — call `read`/`grep` yourself instead.",
		"- The sub-agent has zero context from your conversation. Brief it like a colleague who walked into the room: what you're trying to accomplish, file paths and symbols already in play, what you've ruled out, expected output shape. Terse one-liners produce shallow results.",
		"- State explicitly whether the sub-agent may edit files or is read-only. If you want a short reply, say so (e.g. \"report in under 200 words\").",
		"- Do not delegate synthesis — read the report and integrate findings yourself.",
	}, "\n")

	return []tool.Tool{{
		Name:        "agent",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectMutates),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"description": map[string]any{"type": "string", "description": "3-5 word label for the UI (e.g. \"Audit auth middleware\")."},
				"prompt":      map[string]any{"type": "string", "description": "Self-contained task briefing."},
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
