package code

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

// Runs the real edit tool against the gopls on PATH, as the agent does.
func TestEditReportsDiagnosticsFromInstalledGopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not installed")
	}
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	t.Setenv("GOFLAGS", "-mod=mod")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.WarmUp()

	var edit tool.Tool
	for _, candidate := range w.WithLanguageSync(fs.Tools(w.Root, nil)) {
		if candidate.Name == "edit" {
			edit = candidate
		}
	}
	run := func(args map[string]any) string {
		t.Helper()
		started := time.Now()
		result, err := edit.Execute(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("edit took %s", time.Since(started).Round(time.Millisecond))
		return result.Content.Text()
	}

	if w.LanguageService().Running() {
		t.Fatal("a language server runs before any edit")
	}
	text := run(map[string]any{"file_path": "main.go", "old_string": "", "new_string": "package main\n\nfunc main() {\n\tvar n int = \"text\"\n\t_ = n\n}\n"})
	if !strings.Contains(text, "lsp diagnostics for main.go") || !strings.Contains(text, "main.go:4:") {
		t.Fatalf("the first edit reported no diagnostics:\n%s", text)
	}
	text = run(map[string]any{"file_path": "main.go", "old_string": "var n int = \"text\"", "new_string": "var n int = 1"})
	if strings.Contains(text, "lsp diagnostics") {
		t.Fatalf("a fixed file still reports diagnostics:\n%s", text)
	}

	path := filepath.Join(root, "main.go")
	draft := "package main\nvar onlyInEditor int = \"draft\"\n"
	if err := w.LanguageService().SyncDocument(t.Context(), path, draft, false); err != nil {
		t.Fatal(err)
	}
	if diagnostics, known, err := w.LanguageService().FileDiagnostics(t.Context(), path, &draft); err != nil || !known || len(diagnostics) == 0 {
		t.Fatalf("expected a diagnostic for the unsaved draft, got %v (known=%v, err=%v)", diagnostics, known, err)
	}
	text = run(map[string]any{"file_path": "main.go", "old_string": "var n int = 1", "new_string": "var n int = 2"})
	if strings.Contains(text, "lsp diagnostics") {
		t.Fatalf("the disk edit reported the draft's diagnostic:\n%s", text)
	}
	if content, ok := w.LanguageService().DocumentContent(path); !ok || content != draft {
		t.Fatalf("the disk edit replaced the editor draft: %q", content)
	}
}
