package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// One tool keeps the rarely used scheduling surface small in every request.
func Tools(store Store, options ...*Options) []tool.Tool {
	var opts *Options
	if len(options) > 0 {
		opts = options[0]
	}
	return []tool.Tool{{
		Name:   "schedule",
		Effect: scheduleEffect,
		Description: strings.Join([]string{
			"Manage scheduled tasks. When a task comes due, you are woken with its prompt.",
			"- `create` takes `prompt` and `schedule`, plus an optional pre-check `script`. `list` shows every task with its status and next run. `pause`, `resume`, and `remove` take a task `id`.",
			"- Schedules use local time: an interval (\"every 15m\", counted from the last run, at least 10s), a 5-field cron expression (\"0 9 * * 1-5\"), or a one-time timestamp (\"2026-04-15T09:00\" or RFC 3339) that is removed after it runs.",
			"- Every fire costs a model turn. For frequent checks, add a `script` so most fires skip silently. When the requested time is approximate, avoid the :00 and :30 marks (\"57 8 * * *\" rather than \"0 9 * * *\").",
		}, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"create", "list", "pause", "resume", "remove"},
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "create: what the task should do when it runs.",
				},
				"schedule": map[string]any{
					"type":        "string",
					"description": "create: interval, cron expression, or timestamp.",
				},
				"script": map[string]any{
					"type":        "string",
					"description": "create: pre-check run with the `exec_command` shell before each fire. Print {\"wake\": false} to skip the run; any other output, or a failure, wakes you with the output attached. Test it with `exec_command` first.",
				},
				"id": map[string]any{
					"type":        "string",
					"description": "pause, resume, remove: task ID or a unique prefix.",
				},
			},
			"required":             []string{"action"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			switch action, _ := args["action"].(string); action {
			case "create":
				return createTask(ctx, store, opts, args)
			case "list":
				return listTasks(store)
			case "pause":
				return updateStatus(store, args, StatusPaused)
			case "resume":
				return updateStatus(store, args, StatusActive)
			case "remove":
				return removeTask(store, args)
			default:
				return tool.Result{}, fmt.Errorf("action must be create, list, pause, resume, or remove (got %q)", action)
			}
		},
	}}
}

func scheduleEffect(args map[string]any) tool.Effect {
	if args == nil {
		return tool.EffectDynamic
	}
	if action, _ := args["action"].(string); action == "list" {
		return tool.EffectReadOnly
	}
	return tool.EffectMutates
}

func createTask(ctx context.Context, store Store, opts *Options, args map[string]any) (tool.Result, error) {
	prompt, _ := args["prompt"].(string)
	sched, _ := args["schedule"].(string)
	script, _ := args["script"].(string)

	task, err := NewTask(prompt, sched)
	if err != nil {
		return tool.Result{}, err
	}

	task.Script = strings.TrimSpace(script)
	dir := ""
	if opts != nil {
		dir = opts.WorkDir
	}
	if err := approveScript(ctx, dir, task.Script, opts); err != nil {
		return tool.Result{}, err
	}
	if task.Script != "" {
		task.ScriptApproval = scriptApproval(dir, task.Script)
	}

	err = store.Mutate(func(tasks []Task) ([]Task, error) {
		return append(tasks, task), nil
	})
	if err != nil {
		return tool.Result{}, err
	}

	now := time.Now()
	return tool.Text(fmt.Sprintf("Task %s scheduled (%s), next run %s: %s", task.ID, task.Schedule, formatNext(NextAttempt(task, now), now), task.Prompt)), nil
}

func listTasks(store Store) (tool.Result, error) {
	tasks, err := store.List()
	if err != nil {
		return tool.Result{}, err
	}

	if len(tasks) == 0 {
		return tool.Text("No tasks scheduled."), nil
	}

	now := time.Now()
	var b strings.Builder

	for _, t := range tasks {
		fmt.Fprintf(&b, "- [%s] %s (schedule: %s, status: %s, next: %s",
			t.ID, t.Prompt, t.Schedule, t.Status, formatNext(NextAttempt(t, now), now))
		if t.Script != "" {
			b.WriteString(", pre-check script")
		}
		if t.LastRun != nil {
			fmt.Fprintf(&b, ", last run: %s", t.LastRun.Local().Format(time.RFC3339))
		}
		if t.Failures > 0 {
			fmt.Fprintf(&b, ", consecutive failures: %d (retrying with backoff)", t.Failures)
		}
		b.WriteString(")\n")
	}

	return tool.Text(b.String()), nil
}

func removeTask(store Store, args map[string]any) (tool.Result, error) {
	id, _ := args["id"].(string)

	var removed string
	err := store.Mutate(func(tasks []Task) ([]Task, error) {
		i, err := Find(tasks, id)
		if err != nil {
			return nil, err
		}
		removed = tasks[i].ID
		return append(tasks[:i:i], tasks[i+1:]...), nil
	})
	if err != nil {
		return tool.Result{}, err
	}

	return tool.Text(fmt.Sprintf("Task %s removed.", removed)), nil
}

func updateStatus(store Store, args map[string]any, status string) (tool.Result, error) {
	id, _ := args["id"].(string)

	var updated string
	err := store.Mutate(func(tasks []Task) ([]Task, error) {
		i, err := Find(tasks, id)
		if err != nil {
			return nil, err
		}
		tasks[i].Status = status
		updated = tasks[i].ID
		return tasks, nil
	})
	if err != nil {
		return tool.Result{}, err
	}

	return tool.Text(fmt.Sprintf("Task %s %s.", updated, status)), nil
}

func formatNext(next, now time.Time) string {
	if next.IsZero() {
		return "n/a"
	}
	return fmt.Sprintf("%s (%s)", next.Local().Format(time.RFC3339), Relative(next, now))
}

// Relative renders a next-run time the way every surface shows it: "overdue",
// "in 42m", "in 3h5m".
func Relative(next, now time.Time) string {
	if next.IsZero() {
		return "never"
	}

	d := next.Sub(now)
	switch {
	case d < 0:
		return "overdue"
	case d < time.Minute:
		return "in <1m"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("in %dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
