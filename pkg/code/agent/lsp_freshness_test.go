package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

func TestSessionLSPPreviewProtectsOrdinaryEdit(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := filepath.Join(dir, "sample.txt")
	before := "old\noriginal\n"
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	freshness := fs.NewFreshness(root)
	s := &sessionState{
		parent:    &Agent{workspace: &code.Workspace{}},
		freshness: freshness,
		tools:     fs.Tools(root, &fs.Options{Freshness: freshness}),
	}
	s.setMode(modeAgent)
	s.tools = append(s.tools, tool.Tool{Name: "lsp", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Result{Content: "preview", Metadata: map[string]any{tool.FileSnapshotsMetadata: []tool.FileSnapshot{{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(before)))}}}}, nil
	}})
	s.harness = &harness.Agent{Config: &harness.Config{Tools: s.catalogTools, FilterTools: s.filterTools}}
	var preview, edit tool.Tool
	for _, candidate := range s.harness.AvailableTools() {
		switch candidate.Name {
		case "lsp":
			preview = candidate
		case "edit":
			edit = candidate
		}
	}
	if _, err := preview.Execute(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\nexternal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := edit.Execute(t.Context(), map[string]any{"file_path": "sample.txt", "old_string": "old", "new_string": "new"}); err == nil || !strings.Contains(err.Error(), "content changed since the preview") {
		t.Fatalf("session did not enforce the private preview snapshot: %v", err)
	}
	s.setMode(modePlan)
	for _, candidate := range s.harness.AvailableTools() {
		if candidate.Name == "edit" {
			t.Fatal("preview tracking exposed edit in plan mode")
		}
	}
}
