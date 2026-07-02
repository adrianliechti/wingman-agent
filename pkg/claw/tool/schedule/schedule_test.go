package schedule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestScheduleTaskRejectsNonPositiveInterval(t *testing.T) {
	scheduleTool := findTool(t, "schedule_task")

	_, err := scheduleTool.Execute(context.Background(), map[string]any{
		"prompt":   "run",
		"schedule": "every 0s",
	})
	if err == nil || !strings.Contains(err.Error(), "duration must be positive") {
		t.Fatalf("expected positive duration error, got: %v", err)
	}
}

func TestScheduleToolsExposeEffects(t *testing.T) {
	tests := map[string]tool.Effect{
		"schedule_task": tool.EffectMutates,
		"list_tasks":    tool.EffectReadOnly,
		"pause_task":    tool.EffectMutates,
		"resume_task":   tool.EffectMutates,
		"remove_task":   tool.EffectMutates,
		"run_task":      tool.EffectMutates,
	}

	for _, tl := range Tools(t.TempDir()) {
		want, ok := tests[tl.Name]
		if !ok {
			t.Fatalf("unexpected tool %q", tl.Name)
		}
		if tl.Effect == nil {
			t.Fatalf("%s effect is nil", tl.Name)
		}
		if got := tl.Effect(nil); got != want {
			t.Fatalf("%s effect = %v, want %v", tl.Name, got, want)
		}
	}
}

func TestMutateCreatesAgentDir(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "missing", "agent")

	err := Mutate(agentDir, func(tasks []Task) ([]Task, error) {
		return append(tasks, Task{
			ID:        "task-1",
			Prompt:    "run",
			Schedule:  "every 1h",
			Status:    "active",
			CreatedAt: time.Now().UTC(),
		}), nil
	})
	if err != nil {
		t.Fatalf("Mutate failed: %v", err)
	}

	got, err := List(agentDir)
	if err != nil || len(got) != 1 || got[0].ID != "task-1" {
		t.Fatalf("List = %#v, %v, want task-1", got, err)
	}
}

func TestListReportsMalformedYAML(t *testing.T) {
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, tasksFile), []byte("tasks: ["), 0644); err != nil {
		t.Fatalf("write tasks file: %v", err)
	}

	if _, err := List(agentDir); err == nil {
		t.Fatal("expected malformed YAML error")
	}
}

func TestCronTaskFiresWithoutLastRun(t *testing.T) {
	created := time.Date(2026, 6, 12, 8, 0, 0, 0, time.UTC)
	task := Task{
		ID:        "cron-1",
		Prompt:    "daily standup",
		Schedule:  "0 9 * * *",
		Status:    "active",
		CreatedAt: created,
	}

	if IsDue(task, created.Add(30*time.Minute)) {
		t.Fatal("task should not be due before its first cron slot")
	}
	if !IsDue(task, created.Add(2*time.Hour)) {
		t.Fatal("task should be due after its first cron slot passed")
	}

	lastRun := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	task.LastRun = &lastRun
	if IsDue(task, lastRun.Add(2*time.Hour)) {
		t.Fatal("task should not be due again before the next cron slot")
	}
	if !IsDue(task, lastRun.Add(25*time.Hour)) {
		t.Fatal("task should be due after the next cron slot passed")
	}
}

func TestParseScheduleAcceptsLocalTimestamp(t *testing.T) {
	for _, sched := range []string{"2026-04-15T09:00", "2026-04-15T09:00:00"} {
		p, err := parseSchedule(sched)
		if err != nil {
			t.Fatalf("parseSchedule(%q): %v", sched, err)
		}
		want := time.Date(2026, 4, 15, 9, 0, 0, 0, time.Local)
		if !p.once.Equal(want) {
			t.Fatalf("parseSchedule(%q) = %v, want %v", sched, p.once, want)
		}
	}
}

func TestRunGate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if wake, _ := RunGate(ctx, dir, `echo '{"wake": false}'`); wake {
		t.Fatal("wake=false output should skip the run")
	}
	if wake, _ := RunGate(ctx, dir, `echo '{"wake": true, "data": [1]}'`); !wake {
		t.Fatal("wake=true output should wake the agent")
	}
	if wake, out := RunGate(ctx, dir, `echo checking; echo '{"wake": false}'`); wake || !strings.Contains(out, "checking") {
		t.Fatalf("trailing JSON line should be honored, got wake=%v out=%q", wake, out)
	}
	if wake, _ := RunGate(ctx, dir, `echo not-json`); !wake {
		t.Fatal("non-JSON output should fail open")
	}
	if wake, out := RunGate(ctx, dir, `exit 3`); !wake || !strings.Contains(out, "failed") {
		t.Fatalf("script failure should fail open with a note, got wake=%v out=%q", wake, out)
	}
}

func findTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	for _, tl := range Tools(t.TempDir()) {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)
	return tool.Tool{}
}
