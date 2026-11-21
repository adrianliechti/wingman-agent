package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/codemode"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

// guardedToolbox calls the session's tools as published, mode guards included.
type guardedToolbox struct{ tools []tool.Tool }

func (b guardedToolbox) Tools() []tool.Tool { return b.tools }

func (b guardedToolbox) Call(ctx context.Context, name string, args map[string]any) tool.Result {
	for _, t := range b.tools {
		if t.Name == name {
			result, err := t.Execute(ctx, args)
			if err != nil {
				return tool.Error("error: " + err.Error())
			}
			return result
		}
	}
	return tool.Error("error: unknown tool " + name)
}

func TestCodeModeFollowsPlanMode(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &sessionState{parent: &Agent{workspace: &code.Workspace{}}}
	s.tools = append(fs.Tools(root, &fs.Options{}), codemode.Tools(&codemode.Options{
		CacheDir: filepath.Join(os.TempDir(), "wingman-codemode-test"),
	})...)
	s.harness = &harness.Agent{Config: &harness.Config{Tools: s.catalogTools, FilterTools: s.filterTools}}

	script := `
before = await read(file_path="sample.txt")
try:
    await edit(edits=[{"file_path": "sample.txt", "old_string": "old", "new_string": "new"}])
    outcome = "edited"
except RuntimeError as e:
    outcome = str(e)
[("old" in before), outcome]
`
	run := func(tools []tool.Tool) string {
		t.Helper()
		index := slices.IndexFunc(tools, func(t tool.Tool) bool { return t.Name == codemode.Name })
		if index < 0 {
			t.Fatal("code tool is not offered")
		}
		result, err := tools[index].Execute(tool.WithToolbox(t.Context(), guardedToolbox{tools}), map[string]any{"code": script})
		if err != nil {
			t.Fatal(err)
		}
		return result.Content.Text()
	}

	s.setMode(modePlan)
	if out := run(s.harness.AvailableTools()); !strings.Contains(out, "plan mode only allows read-only tool calls") || !strings.HasPrefix(out, "[true,") {
		t.Fatalf("plan mode: %s", out)
	}
	if data, _ := os.ReadFile(path); string(data) != "old\n" {
		t.Fatalf("plan mode changed the file: %q", data)
	}
	// A plan-mode subagent keeps the tool but none of the tools that change things.
	if out := run(s.subagentFilterTools(s.catalogTools())); !strings.HasPrefix(out, "The script did not run") || !strings.Contains(out, "edit") {
		t.Fatalf("plan-mode subagent: %s", out)
	}

	s.setMode(modeAgent)
	if out := run(s.harness.AvailableTools()); !strings.HasPrefix(out, `[true,"edited"]`) {
		t.Fatalf("agent mode: %s", out)
	}
	if data, _ := os.ReadFile(path); string(data) != "new\n" {
		t.Fatalf("agent mode did not edit the file: %q", data)
	}
}
