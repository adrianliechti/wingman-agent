package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/task"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func taskTools(tasks *task.Registry) []tool.Tool {
	return []tool.Tool{
		{
			Name:        "agent_output",
			Description: "Check this session's tracked agents, background and synchronous. Without `id`, lists them all with their status. With `id`, returns that agent's status and, once finished, its result. Background results also arrive automatically as task notifications; synchronous results return inline. Large results may be truncated by the shared output limit. Never poll in a loop to wait; continue independent work while background agents run.",
			Effect:      tool.StaticEffect(tool.EffectReadOnly),
			Hidden:      true,

			Parameters: map[string]any{
				"type": "object",

				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Task id of a tracked agent. Omit to list all."},
				},

				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
				if err := ctx.Err(); err != nil {
					return tool.Result{}, err
				}
				raw, present := args["id"]
				if !present {
					return tool.Text(formatTaskList(tasks)), nil
				}
				id, ok := raw.(string)
				id = strings.TrimSpace(id)
				if !ok || id == "" {
					return tool.Result{}, fmt.Errorf("id must be a non-empty string; omit it to list agents")
				}

				t := tasks.Get(id)
				if t == nil {
					return tool.Result{}, fmt.Errorf("no agent with id %s", id)
				}

				info := t.Info()
				if info.Status == task.StatusRunning {
					out := fmt.Sprintf("%s — still running.", formatTaskLine(info))
					if info.Activity != "" {
						out += fmt.Sprintf(" Currently: %s.", info.Activity)
					}
					return tool.Text(out), nil
				}
				return tool.Text(fmt.Sprintf("%s\n\n%s", formatTaskLine(info), info.Result)), nil
			},
		},
		{
			Name:        "agent_send",
			Description: "Send a follow-up task to a finished, tracked agent, including one that failed or was stopped. It resumes in the background with its retained context and the reply arrives as a task notification. Running agents cannot receive messages through this tool. Use this when the question builds on an agent's prior work. Verification of its findings needs a fresh agent without the finder's context.",
			Effect:      tool.StaticEffect(tool.EffectMutates),

			Parameters: map[string]any{
				"type": "object",

				"properties": map[string]any{
					"id":      map[string]any{"type": "string", "description": "Task id of the finished agent to continue."},
					"message": map[string]any{"type": "string", "description": "The follow-up task or question. The agent keeps its earlier context; only state what is new."},
				},

				"required":             []string{"id", "message"},
				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
				if err := ctx.Err(); err != nil {
					return tool.Result{}, err
				}
				id, _ := args["id"].(string)
				id = strings.TrimSpace(id)
				if id == "" {
					return tool.Result{}, fmt.Errorf("id is required")
				}
				message, _ := args["message"].(string)
				if strings.TrimSpace(message) == "" {
					return tool.Result{}, fmt.Errorf("message is required")
				}

				t := tasks.Get(id)
				if t == nil {
					return tool.Result{}, fmt.Errorf("no agent with id %s", id)
				}
				if err := t.ResumeContext(ctx, message); err != nil {
					return tool.Result{}, err
				}
				return tool.Text(fmt.Sprintf("Follow-up sent to background agent %s; it resumed with its prior context. The reply arrives as a task notification — never assume or invent it.", id)), nil
			},
		},
		{
			Name:        "agent_stop",
			Description: "Request cancellation of a running tracked agent. It remains running until cancellation settles; partial output is retained as incomplete and completed edits are not undone. Background runs deliver a stopped-task notification; synchronous runs return to their caller.",
			Effect:      tool.StaticEffect(tool.EffectMutates),

			Parameters: map[string]any{
				"type": "object",

				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Task id of the running agent to stop."},
				},

				"required":             []string{"id"},
				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
				if err := ctx.Err(); err != nil {
					return tool.Result{}, err
				}
				id, _ := args["id"].(string)
				id = strings.TrimSpace(id)
				if id == "" {
					return tool.Result{}, fmt.Errorf("id is required")
				}
				if err := tasks.Stop(id); err != nil {
					return tool.Result{}, err
				}
				return tool.Text(fmt.Sprintf("Stop requested for agent %s.", id)), nil
			},
		},
	}
}

func formatTaskList(tasks *task.Registry) string {
	list := tasks.List()
	if len(list) == 0 {
		return "No tracked agents in this session."
	}

	var b strings.Builder
	for _, t := range list {
		info := t.Info()
		line := formatTaskLine(info)
		if info.Status == task.StatusRunning && info.Activity != "" {
			line += " — " + info.Activity
		}
		fmt.Fprintf(&b, "%s\n", line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTaskLine(t task.Info) string {
	return fmt.Sprintf("%s [%s] %s (%s, %s)", t.ID, t.Status, t.Description, t.AgentType, t.Elapsed.Round(time.Second))
}
