package schedule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const tasksFile = "tasks.yaml"

type Task struct {
	ID        string     `yaml:"id"`
	Prompt    string     `yaml:"prompt"`
	Schedule  string     `yaml:"schedule"`
	Status    string     `yaml:"status"`
	CreatedAt time.Time  `yaml:"created_at"`
	LastRun   *time.Time `yaml:"last_run,omitempty"`

	Failures    int        `yaml:"failures,omitempty"`
	LastAttempt *time.Time `yaml:"last_attempt,omitempty"`
}

type taskFile struct {
	Tasks []Task `yaml:"tasks"`
}

var idParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id": map[string]any{
			"type":        "string",
			"description": "Task ID.",
		},
	},
	"required":             []string{"id"},
	"additionalProperties": false,
}

func taskID(args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	return id, nil
}

func Tools(agentDir string) []tool.Tool {
	return []tool.Tool{
		{
			Name:   "schedule_task",
			Effect: tool.StaticEffect(tool.EffectMutates),
			Description: strings.Join([]string{
				"Schedule a recurring or one-time task.",
				"",
				"Schedule formats:",
				"- Interval: \"every 15m\", \"every 2h\", \"every 24h\"",
				"- Cron: \"0 9 * * 1-5\" (weekdays at 9am), \"*/15 * * * *\" (every 15 min)",
				"- One-time: ISO 8601 timestamp (e.g. \"2026-04-15T09:00:00Z\")",
			}, "\n"),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "What the task should do when it runs.",
					},
					"schedule": map[string]any{
						"type":        "string",
						"description": "Schedule expression: \"every 15m\", cron expression, or ISO 8601 timestamp.",
					},
				},
				"required":             []string{"prompt", "schedule"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				prompt, _ := args["prompt"].(string)
				sched, _ := args["schedule"].(string)

				task, err := NewTask(prompt, sched)
				if err != nil {
					return "", err
				}

				err = Mutate(agentDir, func(tasks []Task) ([]Task, error) {
					return append(tasks, task), nil
				})
				if err != nil {
					return "", err
				}

				return fmt.Sprintf("Task %s scheduled (%s): %s", task.ID, task.Schedule, task.Prompt), nil
			},
		},
		{
			Name:        "list_tasks",
			Description: "List all scheduled tasks with their status and next run time.",
			Effect:      tool.StaticEffect(tool.EffectReadOnly),
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				tasks, err := List(agentDir)
				if err != nil {
					return "", err
				}

				if len(tasks) == 0 {
					return "No tasks scheduled.", nil
				}

				now := time.Now()
				var b strings.Builder

				for _, t := range tasks {
					next := NextRun(t, now)
					nextStr := "n/a"
					if !next.IsZero() {
						nextStr = next.Format(time.RFC3339)
					}

					fmt.Fprintf(&b, "- [%s] %s (schedule: %s, status: %s, next: %s",
						t.ID, t.Prompt, t.Schedule, t.Status, nextStr)
					if t.LastRun != nil {
						fmt.Fprintf(&b, ", last run: %s", t.LastRun.Format(time.RFC3339))
					}
					if t.Failures > 0 {
						fmt.Fprintf(&b, ", consecutive failures: %d (retrying with backoff)", t.Failures)
					}
					b.WriteString(")\n")
				}

				return b.String(), nil
			},
		},
		{
			Name:        "pause_task",
			Description: "Pause a scheduled task by ID.",
			Effect:      tool.StaticEffect(tool.EffectMutates),
			Parameters:  idParams,
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				return updateStatus(agentDir, args, "paused")
			},
		},
		{
			Name:        "resume_task",
			Description: "Resume a paused task by ID.",
			Effect:      tool.StaticEffect(tool.EffectMutates),
			Parameters:  idParams,
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				return updateStatus(agentDir, args, "active")
			},
		},
		{
			Name:        "remove_task",
			Description: "Remove a scheduled task by ID.",
			Effect:      tool.StaticEffect(tool.EffectMutates),
			Parameters:  idParams,
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				id, err := taskID(args)
				if err != nil {
					return "", err
				}

				err = Mutate(agentDir, func(tasks []Task) ([]Task, error) {
					var kept []Task
					for _, t := range tasks {
						if t.ID != id {
							kept = append(kept, t)
						}
					}
					if len(kept) == len(tasks) {
						return nil, fmt.Errorf("task %s not found", id)
					}
					return kept, nil
				})
				if err != nil {
					return "", err
				}

				return fmt.Sprintf("Task %s removed.", id), nil
			},
		},
		{
			Name:        "run_task",
			Description: "Run a scheduled task immediately, regardless of its schedule. Useful for testing.",
			Effect:      tool.StaticEffect(tool.EffectMutates),
			Parameters:  idParams,
			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				id, err := taskID(args)
				if err != nil {
					return "", err
				}

				var prompt string
				err = Mutate(agentDir, func(tasks []Task) ([]Task, error) {
					for i := range tasks {
						if tasks[i].ID == id {
							now := time.Now()
							tasks[i].LastRun = &now
							prompt = tasks[i].Prompt
							return tasks, nil
						}
					}
					return nil, fmt.Errorf("task %s not found", id)
				})
				if err != nil {
					return "", err
				}

				return fmt.Sprintf("Task triggered. Execute now:\n\n%s", prompt), nil
			},
		},
	}
}

var dirLocks sync.Map

func dirLock(agentDir string) *sync.Mutex {
	mu, _ := dirLocks.LoadOrStore(filepath.Clean(agentDir), &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func HasTaskFile(agentDir string) bool {
	_, err := os.Stat(filepath.Join(agentDir, tasksFile))
	return err == nil
}

func List(agentDir string) ([]Task, error) {
	mu := dirLock(agentDir)
	mu.Lock()
	defer mu.Unlock()
	return loadTasks(agentDir)
}

func Mutate(agentDir string, fn func([]Task) ([]Task, error)) error {
	mu := dirLock(agentDir)
	mu.Lock()
	defer mu.Unlock()

	tasks, err := loadTasks(agentDir)
	if err != nil {
		return err
	}

	updated, err := fn(tasks)
	if err != nil {
		return err
	}

	return saveTasks(agentDir, updated)
}

func loadTasks(agentDir string) ([]Task, error) {
	data, err := os.ReadFile(filepath.Join(agentDir, tasksFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var f taskFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}

	return f.Tasks, nil
}

func saveTasks(agentDir string, tasks []Task) error {
	out, err := yaml.Marshal(taskFile{Tasks: tasks})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(agentDir, tasksFile)
	tmp, err := os.CreateTemp(agentDir, tasksFile+".tmp-")
	if err != nil {
		return err
	}

	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}

	return os.Rename(tmp.Name(), path)
}

var cronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

type parsedSchedule struct {
	interval time.Duration
	once     time.Time
	cron     cron.Schedule
}

func parseSchedule(sched string) (parsedSchedule, error) {
	if rest, ok := strings.CutPrefix(sched, "every "); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return parsedSchedule{}, fmt.Errorf("invalid interval %q: %w", sched, err)
		}
		if d <= 0 {
			return parsedSchedule{}, fmt.Errorf("invalid interval %q: duration must be positive", sched)
		}
		return parsedSchedule{interval: d}, nil
	}

	if ts, err := time.Parse(time.RFC3339, sched); err == nil {
		return parsedSchedule{once: ts}, nil
	}

	if s, err := cronParser.Parse(sched); err == nil {
		return parsedSchedule{cron: s}, nil
	}

	return parsedSchedule{}, fmt.Errorf("invalid schedule %q: must be \"every <duration>\", a cron expression, or an ISO 8601 timestamp", sched)
}

func NextRun(t Task, now time.Time) time.Time {
	if t.Status != "active" {
		return time.Time{}
	}

	p, err := parseSchedule(t.Schedule)
	if err != nil {
		return time.Time{}
	}

	switch {
	case p.interval > 0:
		if t.LastRun == nil {
			return now
		}
		return t.LastRun.Add(p.interval)
	case !p.once.IsZero():
		if t.LastRun != nil {
			return time.Time{}
		}
		return p.once
	default:
		anchor := t.CreatedAt
		if t.LastRun != nil {
			anchor = *t.LastRun
		}
		return p.cron.Next(anchor)
	}
}

func IsDue(t Task, now time.Time) bool {
	if t.Failures > 0 && t.LastAttempt != nil {
		backoff := min(time.Hour, time.Duration(1<<min(t.Failures, 6))*time.Minute)
		if now.Before(t.LastAttempt.Add(backoff)) {
			return false
		}
	}

	next := NextRun(t, now)
	return !next.IsZero() && !next.After(now)
}

func IsOneTime(sched string) bool {
	p, err := parseSchedule(sched)
	return err == nil && !p.once.IsZero()
}

func NewTask(prompt, sched string) (Task, error) {
	prompt = strings.TrimSpace(prompt)
	sched = strings.TrimSpace(sched)

	if prompt == "" {
		return Task{}, fmt.Errorf("prompt is required")
	}
	if sched == "" {
		return Task{}, fmt.Errorf("schedule is required")
	}
	if _, err := parseSchedule(sched); err != nil {
		return Task{}, err
	}

	return Task{
		ID:        uuid.NewString(),
		Prompt:    prompt,
		Schedule:  sched,
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}, nil
}

func updateStatus(agentDir string, args map[string]any, status string) (string, error) {
	id, err := taskID(args)
	if err != nil {
		return "", err
	}

	err = Mutate(agentDir, func(tasks []Task) ([]Task, error) {
		for i := range tasks {
			if tasks[i].ID == id {
				tasks[i].Status = status
				return tasks, nil
			}
		}
		return nil, fmt.Errorf("task %s not found", id)
	})
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Task %s %s.", id, status), nil
}
