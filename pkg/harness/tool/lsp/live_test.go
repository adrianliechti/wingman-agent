package lsp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

// This test deliberately uses only the public API available before these
// improvements, so the same evaluation can run against an archived baseline.
// It never installs tools or changes the user's project; all edits use fixtures.
func TestLiveLSPAgentEvaluation(t *testing.T) {
	if os.Getenv("WINGMAN_LIVE_LSP_AGENT") == "" {
		t.Skip("set WINGMAN_LIVE_LSP_AGENT=1; requires an installed managed gopls")
	}
	tools, err := devtools.New()
	if err != nil {
		t.Fatal(err)
	}
	if tools.Resolve("gopls") == "" {
		t.Fatal("managed gopls unavailable; this evaluation never installs tools")
	}
	root := t.TempDir()
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	files := map[string]string{
		"go.mod":   "module lsp-evaluation.test\n\ngo 1.26\n",
		"types.go": "package sample\n\ntype Alpha struct{}\nfunc (Alpha) Close() string { return \"alpha\" }\ntype Beta struct{}\nfunc (Beta) Close() string { return \"beta\" }\ntype Item struct { Value int }\nfunc UseItem(item Item) int { return item.Value }\n",
		"refs.go":  "package sample\n\nfunc Target() int { return 42 }\n",
		"decoy.go": "package sample\n\n// Target is also mentioned in documentation.\nvar Text = \"Target\"\nfunc Shadow() int { Target := 7; return Target }\n",
	}
	for i := range 125 {
		files["refs.go"] += fmt.Sprintf("func Use%03d() int { return Target() }\n", i)
	}
	files["types.go"] += "func CrossFile() {}\n"
	files["cross.go"] = "package sample\n\nfunc CallCross() { CrossFile() }\n"
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service := language.New(root, lsp.WithManagedTools(tools))
	defer service.Close()
	inspect := lsptool.NewTools(service)[0]
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	run := func(t *testing.T, args map[string]any) (tool.Result, error) {
		t.Helper()
		start := time.Now()
		result, err := inspect.Execute(ctx, args)
		t.Logf("METRIC operation=%v elapsed_ms=%.3f output_bytes=%d error=%t", args["operation"], float64(time.Since(start).Microseconds())/1000, len(result.Content), err != nil)
		return result, err
	}
	t.Run("cold_definition", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "goToDefinition", "file_path": "refs.go", "line": 4, "symbol": "Target"})
		if err != nil || !strings.Contains(result.Content, "refs.go:3:") {
			t.Fatalf("definition=%s error=%v", result.Content, err)
		}
	})
	t.Run("qualified_symbol", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "hover", "file_path": "types.go", "symbol": "Beta.Close"})
		if err != nil || !strings.Contains(result.Content, "Beta") {
			t.Fatalf("hover=%s error=%v", result.Content, err)
		}
	})
	t.Run("qualified_symbol_with_line", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "hover", "file_path": "types.go", "symbol": "Beta.Close", "line": 6})
		if err != nil || !strings.Contains(result.Content, "Beta") {
			t.Fatalf("qualified declaration plus line=%s error=%v", result.Content, err)
		}
		if _, err := run(t, map[string]any{"operation": "hover", "file_path": "types.go", "symbol": "Beta.Close", "line": 4}); err == nil {
			t.Fatal("qualified name accepted at the wrong receiver's line")
		}
	})
	t.Run("ambiguous_symbol", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "hover", "file_path": "types.go", "symbol": "Close"})
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("silently selected a symbol: %s %v", result.Content, err)
		}
	})
	t.Run("wrong_qualified_symbol", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "hover", "file_path": "types.go", "symbol": "Missing.Close"})
		if err == nil {
			t.Fatalf("matched wrong receiver: %s", result.Content)
		}
	})
	t.Run("type_definition", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "goToTypeDefinition", "file_path": "types.go", "line": 8, "column": 14})
		if err != nil || !strings.Contains(result.Content, "types.go:7:") {
			t.Fatalf("type definition=%s %v", result.Content, err)
		}
	})
	t.Run("reference_completeness", func(t *testing.T) {
		var output strings.Builder
		for _, offset := range []int{0, 100} {
			result, err := run(t, map[string]any{"operation": "findReferences", "file_path": "refs.go", "symbol": "Target", "offset": offset, "limit": 100})
			if err != nil {
				t.Fatal(err)
			}
			output.WriteString(result.Content)
		}
		found := 0
		for i := range 125 {
			if strings.Contains(output.String(), fmt.Sprintf("refs.go:%d:", i+4)) {
				found++
			}
		}
		t.Logf("METRIC reference_calls=2 true_references_found=%d expected=125 output_bytes=%d", found, output.Len())
		if found != 125 || strings.Contains(output.String(), "decoy.go:") {
			t.Fatalf("reference recall=%d/125; decoys included=%t", found, strings.Contains(output.String(), "decoy.go:"))
		}
	})
	t.Run("compact_reference_counts", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "findReferences", "file_path": "refs.go", "symbol": "Target", "output_mode": "count", "include_declaration": false})
		if err != nil || !strings.Contains(result.Content, "125 across 1 files") || !strings.Contains(result.Content, "refs.go: 125") || strings.Contains(result.Content, "decoy.go") {
			t.Fatalf("counts=%s error=%v", result.Content, err)
		}
	})
	t.Run("preview_and_apply_rename", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "previewRename", "file_path": "refs.go", "symbol": "Target", "new_name": "Renamed"})
		if err != nil {
			t.Fatal(err)
		}
		for name, contents := range files {
			data, _ := os.ReadFile(filepath.Join(root, name))
			if string(data) != contents {
				t.Fatalf("preview changed %s", name)
			}
		}
		index := strings.Index(result.Content, "{")
		if index < 0 {
			t.Fatal(result.Content)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(result.Content[index:]), &args); err != nil {
			t.Fatal(err)
		}
		fsRoot, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer fsRoot.Close()
		for _, edit := range fs.Tools(fsRoot, nil) {
			if edit.Name == "edit" {
				if _, err := edit.Execute(ctx, args); err != nil {
					t.Fatal(err)
				}
			}
		}
		data, _ := os.ReadFile(filepath.Join(root, "refs.go"))
		if strings.Count(string(data), "Renamed") != 126 || strings.Contains(string(data), "Target") {
			t.Fatalf("rename incomplete")
		}
		data, _ = os.ReadFile(filepath.Join(root, "decoy.go"))
		if string(data) != files["decoy.go"] {
			t.Fatal("rename changed unrelated same-name text")
		}
	})
	t.Run("code_actions", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "codeActions", "file_path": "types.go"})
		if err != nil || !strings.Contains(result.Content, "Code actions") {
			t.Fatalf("code actions=%s %v", result.Content, err)
		}
	})
	t.Run("preview_across_unopened_files", func(t *testing.T) {
		result, err := run(t, map[string]any{"operation": "previewRename", "file_path": "types.go", "symbol": "CrossFile", "new_name": "RenamedCross"})
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Edits []struct {
				Path string `json:"file_path"`
			} `json:"edits"`
		}
		index := strings.Index(result.Content, "{")
		if index < 0 {
			t.Fatal("missing preview JSON")
		}
		if err := json.Unmarshal([]byte(result.Content[index:]), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Edits) != 2 {
			t.Fatalf("expected two files, got %+v", payload.Edits)
		}
		for _, edit := range payload.Edits {
			data, err := os.ReadFile(filepath.Join(root, edit.Path))
			if err != nil || string(data) != files[edit.Path] {
				t.Fatalf("preview changed %s", edit.Path)
			}
		}
	})
	t.Run("missing_server", func(t *testing.T) {
		missing := language.New(root)
		defer missing.Close()
		result, err := lsptool.NewTools(missing)[0].Execute(ctx, map[string]any{"operation": "hover", "file_path": "types.go", "symbol": "Item"})
		if err == nil || !strings.Contains(err.Error(), "grep/glob/read") {
			t.Fatalf("missing fallback: %s %v", result.Content, err)
		}
	})
}
