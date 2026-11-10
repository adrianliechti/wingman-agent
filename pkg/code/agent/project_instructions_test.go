package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func writeInstructions(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loadedBytes reads the byte counts back from a truncation warning.
func loadedBytes(t *testing.T, warning, path string) (loaded, size int) {
	t.Helper()
	if _, err := fmt.Sscanf(warning, "Loaded only %d of %d bytes from", &loaded, &size); err != nil || !strings.Contains(warning, " bytes from "+path+": ") {
		t.Fatalf("warning %q does not report the bytes loaded from %s: %v", warning, path, err)
	}
	return loaded, size
}

func TestProjectInstructionsTruncateOnRuneBoundary(t *testing.T) {
	// Every padding moves the byte budget onto a different byte of a
	// multi-byte character in a file without line breaks.
	for padding := range 4 {
		root := t.TempDir()
		path := filepath.Join(root, "AGENTS.md")
		content := strings.Repeat("a", padding) + strings.Repeat("日本語🙂é", projectInstructionsMaxBytes/8)
		writeInstructions(t, path, content)

		var cache projectInstructionCache
		rendered, warnings := cache.load(root)
		if !utf8.ValidString(rendered) {
			t.Fatalf("padding %d: truncation split a character", padding)
		}
		body, ok := strings.CutSuffix(rendered, "\n\n[truncated]")
		if !ok || len(body) > projectInstructionsMaxBytes {
			t.Fatalf("padding %d: %d bytes rendered, truncated=%t", padding, len(body), ok)
		}
		kept := strings.TrimPrefix(body, "From AGENTS.md:\n\n")
		if !strings.HasPrefix(content, kept) {
			t.Fatalf("padding %d: rendered text is not a prefix of the file", padding)
		}
		// A single long line is still loaded up to the budget, within one character.
		if len(body) < projectInstructionsMaxBytes-utf8.UTFMax {
			t.Fatalf("padding %d: only %d bytes of a %d byte budget were used", padding, len(body), projectInstructionsMaxBytes)
		}
		if len(warnings) != 1 {
			t.Fatalf("padding %d: warnings = %v", padding, warnings)
		}
		if loaded, size := loadedBytes(t, warnings[0], path); loaded != len(kept) || size != len(content) {
			t.Fatalf("padding %d: warning reports %d of %d bytes, loaded %d of %d", padding, loaded, size, len(kept), len(content))
		}
	}
}

func TestProjectInstructionsTruncateOnLineBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	line := "- règle numéro %04d: toujours écrire des tests 日本語\r\n"
	var content strings.Builder
	for i := 0; content.Len() < 2*projectInstructionsMaxBytes; i++ {
		fmt.Fprintf(&content, line, i)
	}
	writeInstructions(t, path, content.String())

	var cache projectInstructionCache
	rendered, warnings := cache.load(root)
	body, ok := strings.CutSuffix(rendered, "\n\n[truncated]")
	if !ok || !utf8.ValidString(rendered) || len(body) > projectInstructionsMaxBytes {
		t.Fatalf("rendered %d bytes, truncated=%t, valid=%t", len(body), ok, utf8.ValidString(rendered))
	}
	kept := strings.TrimPrefix(body, "From AGENTS.md:\n\n")
	if !strings.HasPrefix(content.String(), kept+"\r\n") || !strings.HasSuffix(kept, "日本語") {
		t.Fatalf("truncation did not end on a complete line: %q", kept[max(0, len(kept)-80):])
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v", warnings)
	}
	if loaded, size := loadedBytes(t, warnings[0], path); loaded != len(kept) || size != len(strings.TrimSpace(content.String())) {
		t.Fatalf("warning reports %d of %d bytes, loaded %d", loaded, size, len(kept))
	}
}

func TestProjectInstructionsBudgetWarnsOncePerCause(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	broad, specific := filepath.Join(root, "AGENTS.md"), filepath.Join(sub, "AGENTS.md")
	huge := strings.Repeat("general guidance line\n", projectInstructionsMaxBytes/20)
	writeInstructions(t, broad, huge)
	writeInstructions(t, specific, "subproject-specific rule")

	var cache projectInstructionCache
	rendered, warnings := cache.load(sub)
	if !strings.Contains(rendered, "subproject-specific rule") || strings.Contains(rendered, "general guidance") || !strings.Contains(rendered, "1 broader instruction file(s) omitted") {
		t.Fatalf("rendered:\n%.300s", rendered)
	}
	want := fmt.Sprintf("Skipped %s (%d bytes)", broad, len(strings.TrimSpace(huge)))
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], want) {
		t.Fatalf("warnings = %v, want one starting with %q", warnings, want)
	}
	// Every prompt build renders the instructions again; the warning does not repeat.
	for range 3 {
		if again, warnings := cache.load(sub); again != rendered || len(warnings) != 0 {
			t.Fatalf("repeated load warned again: %v", warnings)
		}
	}

	// A different amount is a different warning.
	writeInstructions(t, broad, huge+huge)
	if _, warnings := cache.load(sub); len(warnings) != 1 || !strings.Contains(warnings[0], fmt.Sprintf("(%d bytes)", len(strings.TrimSpace(huge+huge)))) {
		t.Fatalf("changed omission: %v", warnings)
	}
	// Once the cause is fixed nothing is reported, and its return is new again.
	writeInstructions(t, broad, "short general rule")
	if rendered, warnings := cache.load(sub); !strings.Contains(rendered, "short general rule") || len(warnings) != 0 {
		t.Fatalf("fixed budget: %q %v", rendered, warnings)
	}
	writeInstructions(t, broad, huge)
	if _, warnings := cache.load(sub); len(warnings) != 1 || !strings.HasPrefix(warnings[0], want) {
		t.Fatalf("returned omission: %v", warnings)
	}
}

func TestProjectInstructionsSkipFilesLinkedIntoCredentialStore(t *testing.T) {
	home := testenv.UserHome(t)
	testenv.WingmanHome(t)
	store := filepath.Join(home, ".ssh")
	writeInstructions(t, filepath.Join(store, "config"), "Host credential-fixture")
	root := t.TempDir()
	linked, ordinary := filepath.Join(root, "AGENTS.md"), filepath.Join(root, "CLAUDE.md")
	writeInstructions(t, linked, "ordinary agents rule")
	writeInstructions(t, ordinary, "ordinary claude rule")

	var cache projectInstructionCache
	if rendered, warnings := cache.load(root); !strings.Contains(rendered, "ordinary agents rule") || len(warnings) != 0 {
		t.Fatalf("initial=%q warnings=%v", rendered, warnings)
	}

	// The repository replaces the file with a link after the session started.
	if err := os.Remove(linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "config"), linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rendered, warnings := cache.load(root)
	if strings.Contains(rendered, "credential-fixture") || strings.Contains(rendered, "ordinary agents rule") || !strings.Contains(rendered, "ordinary claude rule") {
		t.Fatalf("rendered after the link appeared:\n%s", rendered)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "skipped instructions from "+linked) || !strings.Contains(warnings[0], "protected credential location") || strings.Contains(warnings[0], "credential-fixture") {
		t.Fatalf("warnings = %v", warnings)
	}
	if again, warnings := cache.load(root); again != rendered || len(warnings) != 0 {
		t.Fatalf("repeated load: %q %v", again, warnings)
	}

	// A whole directory linked into the store is refused the same way.
	nested := filepath.Join(root, "nested")
	if err := os.Symlink(store, nested); err != nil {
		t.Fatal(err)
	}
	writeInstructions(t, filepath.Join(store, "AGENTS.md"), "credential-fixture instructions")
	var fresh projectInstructionCache
	if rendered, warnings := fresh.load(nested); strings.Contains(rendered, "credential-fixture") || len(warnings) != 2 {
		t.Fatalf("workspace path through a linked directory: %q %v", rendered, warnings)
	}

	// Restoring an ordinary file loads it again.
	if err := os.Remove(linked); err != nil {
		t.Fatal(err)
	}
	writeInstructions(t, linked, "restored agents rule")
	if rendered, warnings := cache.load(root); !strings.Contains(rendered, "restored agents rule") || len(warnings) != 0 {
		t.Fatalf("restored=%q warnings=%v", rendered, warnings)
	}
}

type warningCaptureUI struct {
	warnings []string
}

func (*warningCaptureUI) Elicit(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
	return tool.ElicitResult{}, nil
}
func (*warningCaptureUI) Confirm(context.Context, string) (bool, error) { return false, nil }
func (u *warningCaptureUI) Warn(sessionID, message string) {
	u.warnings = append(u.warnings, sessionID+": "+message)
}

func TestProjectInstructionWarningsReachTheUserOncePerSession(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	writeInstructions(t, path, strings.Repeat("guidance line\n", projectInstructionsMaxBytes/10))

	ui := &warningCaptureUI{}
	a := &Agent{workspace: &code.Workspace{RootPath: root}, ui: ui}
	first, second := &sessionState{id: "first", parent: a}, &sessionState{id: "second", parent: a}
	// The instructions are rendered for every prompt build and every subagent.
	for range 3 {
		if rendered := first.projectInstructions(); !strings.HasSuffix(rendered, "[truncated]") {
			t.Fatalf("rendered %.80q", rendered)
		}
	}
	second.projectInstructions()
	if len(ui.warnings) != 2 || !strings.HasPrefix(ui.warnings[0], "first: Loaded only ") || !strings.HasPrefix(ui.warnings[1], "second: Loaded only ") || !strings.Contains(ui.warnings[0], path) {
		t.Fatalf("warnings shown to the user = %q", ui.warnings)
	}

	// Without a front end that shows warnings they stay on stderr.
	a.SetUI(nil)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = writer
	(&sessionState{id: "headless", parent: a}).projectInstructions()
	os.Stderr = stderr
	writer.Close()
	output, _ := io.ReadAll(reader)
	if !strings.HasPrefix(string(output), "warning: Loaded only ") || strings.Count(string(output), "\n") != 1 {
		t.Fatalf("stderr = %q", output)
	}
}
