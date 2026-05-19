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

func TestSaveTasksCreatesAgentDir(t *testing.T) {
	agentDir := filepath.Join(t.TempDir(), "missing", "agent")

	err := SaveTasks(agentDir, []Task{{
		ID:        "task-1",
		Prompt:    "run",
		Schedule:  "every 1h",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatalf("SaveTasks failed: %v", err)
	}

	if got := LoadTasks(agentDir); len(got) != 1 || got[0].ID != "task-1" {
		t.Fatalf("LoadTasks = %#v, want task-1", got)
	}
}

func TestLoadTasksErrorReportsMalformedYAML(t *testing.T) {
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, tasksFile), []byte("tasks: ["), 0644); err != nil {
		t.Fatalf("write tasks file: %v", err)
	}

	_, err := LoadTasksError(agentDir)
	if err == nil {
		t.Fatal("expected malformed YAML error")
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
