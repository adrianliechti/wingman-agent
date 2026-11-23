package lsp_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

// It never installs tools or changes the user's project; it works on fixtures.
func TestLiveLSPAgentEvaluation(t *testing.T) {
	if os.Getenv("WINGMAN_LIVE_LSP_AGENT") == "" {
		t.Skip("set WINGMAN_LIVE_LSP_AGENT=1; requires gopls on PATH")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Fatal("gopls is not on PATH; this evaluation never installs tools")
	}
	root := t.TempDir()
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	files := map[string]string{
		"go.mod":   "module lsp-evaluation.test\n\ngo 1.26\n",
		"types.go": "package sample\n\ntype Alpha struct{}\nfunc (Alpha) Close() string { return \"alpha\" }\ntype Beta struct{}\nfunc (Beta) Close() string { return \"beta\" }\ntype Closer interface{ Close() string }\nfunc CrossFile() {}\n",
		"refs.go":  "package sample\n\nfunc Target() int { return 42 }\n",
		"decoy.go": "package sample\n\n// Target is also mentioned in documentation.\nvar Text = \"Target\"\nfunc Shadow() int { Target := 7; return Target }\n",
		"cross.go": "package sample\n\nfunc CallCross() { CrossFile() }\n",
		"late.go":  "package sample\n\nfunc Late(a int) int { return a }\n",
		"uses.go":  "package sample\n\nvar _ = Late(1)\n",
	}
	for i := range 225 {
		files["refs.go"] += fmt.Sprintf("func Use%03d() int { return Target() }\n", i)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service := language.New(root)
	defer service.Close()
	inspect := lsptool.NewTools(service)[0]
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	run := func(t *testing.T, args map[string]any) (tool.Result, error) {
		t.Helper()
		start := time.Now()
		result, err := inspect.Execute(ctx, args)
		t.Logf("METRIC operation=%v elapsed_ms=%.3f output_bytes=%d error=%t", args["operation"], float64(time.Since(start).Microseconds())/1000, len(result.Content.Text()), err != nil)
		return result, err
	}
	expect := func(t *testing.T, args map[string]any, want ...string) string {
		t.Helper()
		result, err := run(t, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, text := range want {
			if !strings.Contains(result.Content.Text(), text) {
				t.Fatalf("%v = %s, want %q", args, result.Content.Text(), text)
			}
		}
		return result.Content.Text()
	}

	if service.Running() {
		t.Fatal("gopls started before the first query")
	}
	t.Run("cold_definition", func(t *testing.T) {
		expect(t, map[string]any{"operation": "goToDefinition", "filePath": "refs.go", "line": 4, "symbol": "Target"}, "refs.go:3:")
	})
	t.Run("position", func(t *testing.T) {
		expect(t, map[string]any{"operation": "hover", "filePath": "refs.go", "line": 3, "character": 6}, "func Target() int")
	})
	t.Run("qualified_symbol", func(t *testing.T) {
		expect(t, map[string]any{"operation": "hover", "filePath": "types.go", "symbol": "Beta.Close"}, "Beta")
		expect(t, map[string]any{"operation": "hover", "filePath": "types.go", "symbol": "Beta.Close", "line": 6}, "Beta")
		if _, err := run(t, map[string]any{"operation": "hover", "filePath": "types.go", "symbol": "Beta.Close", "line": 4}); err == nil {
			t.Fatal("qualified name accepted at the wrong receiver's line")
		}
		if result, err := run(t, map[string]any{"operation": "hover", "filePath": "types.go", "symbol": "Close"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("silently selected a symbol: %s %v", result.Content.Text(), err)
		}
		if result, err := run(t, map[string]any{"operation": "hover", "filePath": "types.go", "symbol": "Missing.Close"}); err == nil {
			t.Fatalf("matched wrong receiver: %s", result.Content.Text())
		}
	})
	t.Run("references", func(t *testing.T) {
		output := expect(t, map[string]any{"operation": "findReferences", "filePath": "refs.go", "symbol": "Target"},
			"References (226 found across 1 files):", "refs.go:4:28: func Use000() int { return Target() }", "refs.go:228:28: func Use224() int { return Target() }")
		if strings.Contains(output, "decoy.go:") || strings.Count(output, "refs.go:") != 226 {
			t.Fatalf("references are incomplete or include same-named text: %d listed", strings.Count(output, "refs.go:"))
		}
	})
	t.Run("implementation", func(t *testing.T) {
		expect(t, map[string]any{"operation": "goToImplementation", "filePath": "types.go", "symbol": "Closer"}, "Implementations (2 found", "types.go:3:", "types.go:5:")
	})
	t.Run("symbols", func(t *testing.T) {
		expect(t, map[string]any{"operation": "documentSymbol", "filePath": "types.go"}, "(Alpha).Close (Method)", "CrossFile (Function)")
		expect(t, map[string]any{"operation": "workspaceSymbol", "filePath": "types.go", "query": "CrossFile"}, "CrossFile (Function)", "types.go:")
		expect(t, map[string]any{"operation": "workspaceSymbol", "query": "Beta.Close"}, "Close (Method)", "types.go:")
	})
	t.Run("calls", func(t *testing.T) {
		expect(t, map[string]any{"operation": "prepareCallHierarchy", "filePath": "types.go", "symbol": "CrossFile"}, "CrossFile (Function)")
		expect(t, map[string]any{"operation": "incomingCalls", "filePath": "types.go", "symbol": "CrossFile"}, "CallCross (Function) - cross.go:3")
		expect(t, map[string]any{"operation": "outgoingCalls", "filePath": "cross.go", "symbol": "CallCross"}, "CrossFile (Function) - types.go:8")
	})
	t.Run("follows_reported_file_changes", func(t *testing.T) {
		query := map[string]any{"operation": "hover", "filePath": "uses.go", "line": 3, "symbol": "Late"}
		expect(t, query, "func Late(a int) int")
		late := filepath.Join(root, "late.go")
		if err := os.WriteFile(late, []byte("package sample\n\nfunc Late(a int, b string) int { return a }\n"), 0600); err != nil {
			t.Fatal(err)
		}
		// What the agent's edit and shell tools report for the files they change.
		service.FilesChanged(ctx, []lsp.FileChange{{Path: late, Type: lsp.FileChangeTypeChanged}})
		var text string
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			result, err := run(t, query)
			if text = result.Content.Text(); err == nil && strings.Contains(text, "func Late(a int, b string) int") {
				return
			}
		}
		t.Fatalf("hover still answers from the old file: %s", text)
	})
	t.Run("missing_server", func(t *testing.T) {
		absent := lsp.Server{Name: "gopls", Command: filepath.Join(root, "missing-gopls"), ExtensionToLanguage: map[string]string{".go": "go"}}
		missing := language.New(root, absent)
		defer missing.Close()
		result, err := lsptool.NewTools(missing)[0].Execute(ctx, map[string]any{"operation": "hover", "filePath": "types.go", "symbol": "Alpha"})
		if err == nil || !strings.Contains(err.Error(), "not found on PATH") || !strings.Contains(err.Error(), "grep") {
			t.Fatalf("missing fallback: %s %v", result.Content.Text(), err)
		}
	})
}
