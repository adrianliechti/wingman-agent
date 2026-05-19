package code_test

import (
	"context"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	. "github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestPlanModeToolsFilterMutations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	defer ws.Close()

	cfg, err := agent.DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}

	a := ws.NewAgent(cfg, nil)
	a.PlanMode = true

	tools := a.Config.Tools()
	names := make(map[string]bool)
	for _, current := range tools {
		names[current.Name] = true
		if current.Name == "edit" || current.Name == "write" {
			t.Fatalf("plan mode exposed mutating tool %q", current.Name)
		}
	}
	if !names["read"] || !names["shell"] {
		t.Fatalf("plan mode tools missing read/shell, got %#v", names)
	}
}

func TestPlanModeShellRejectsMutatingSafeCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	defer ws.Close()

	cfg, err := agent.DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}

	a := ws.NewAgent(cfg, nil)
	a.PlanMode = true

	var shellTool *tool.Tool
	tools := a.Config.Tools()
	for i := range tools {
		if tools[i].Name == "shell" {
			shellTool = &tools[i]
			break
		}
	}
	if shellTool == nil {
		t.Fatal("shell tool missing")
	}

	for _, command := range []string{
		"echo hi > file.txt",
		"cat <<'EOF'\nhello\nEOF",
		"sed -i 's/a/b/' file.txt",
		"sed --in-place 's/a/b/' file.txt",
	} {
		t.Run(command, func(t *testing.T) {
			_, err := shellTool.Execute(context.Background(), map[string]any{"command": command})
			if err == nil || !strings.Contains(err.Error(), "plan mode only allows read-only tool calls") {
				t.Fatalf("command was not rejected with plan-mode error: %v", err)
			}
		})
	}
}
