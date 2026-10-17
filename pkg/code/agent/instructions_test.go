package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalShellAndTimezoneFromEnvironment(t *testing.T) {
	t.Setenv("SHELL", filepath.Join("opt", "bin", "zsh"))
	t.Setenv("TZ", "Europe/Berlin")

	if got := localShell(); got != "zsh" {
		t.Fatalf("localShell() = %q, want zsh", got)
	}
	if got := localTimezone(time.Now()); got != "Europe/Berlin" {
		t.Fatalf("localTimezone() = %q, want Europe/Berlin", got)
	}
}

func TestProjectInstructionsRootFirstAndDeduped(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(root, "AGENTS.md"), "root rules")
	write(filepath.Join(sub, "AGENTS.md"), "sub rules")
	write(filepath.Join(sub, "CLAUDE.md"), "sub rules")

	found := findProjectInstructions(sub)
	if len(found) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(found))
	}
	if filepath.Dir(found[0].path) != root {
		t.Fatalf("expected root-level file first, got %s", found[0].path)
	}
	if filepath.Base(found[1].path) != "AGENTS.md" {
		t.Fatalf("expected AGENTS.md before CLAUDE.md within a directory, got %s", found[1].path)
	}

	rendered, files, warnings := renderProjectInstructions(found, nil)
	if len(files) != 3 || len(warnings) != 0 {
		t.Fatalf("cached files=%d warnings=%v", len(files), warnings)
	}
	if strings.Count(rendered, "sub rules") != 1 {
		t.Fatalf("duplicate CLAUDE.md content not deduped:\n%s", rendered)
	}
	if strings.Index(rendered, "root rules") > strings.Index(rendered, "sub rules") {
		t.Fatalf("root guidance should precede the more specific file:\n%s", rendered)
	}
}

func TestProjectInstructionsBudgetKeepsMostSpecific(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	huge := strings.Repeat("general guidance line\n", projectInstructionsMaxBytes/20)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(huge), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("subproject-specific rule"), 0644); err != nil {
		t.Fatal(err)
	}

	rendered, _, _ := renderProjectInstructions(findProjectInstructions(sub), nil)
	if !strings.Contains(rendered, "subproject-specific rule") {
		t.Fatal("the most specific instruction file must survive the budget cut")
	}
	if !strings.Contains(rendered, "omitted") {
		t.Fatalf("expected an omission notice, got:\n%.200s", rendered)
	}
}

func TestProjectInstructionsRetainFailedSourcesAndRefreshOthers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	other := filepath.Join(root, "CLAUDE.md")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(path, "old-a")
	write(other, "rule-b")
	var cache projectInstructionCache
	initial, warnings := cache.load(root)
	if !strings.Contains(initial, "old-a") || !strings.Contains(initial, "rule-b") || len(warnings) != 0 {
		t.Fatalf("initial=%q warnings=%v", initial, warnings)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "saved")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	retained, warnings := cache.load(root)
	if !strings.Contains(retained, "old-a") || strings.Contains(retained, "rule-b") || len(warnings) != 1 || !strings.Contains(warnings[0], path) {
		t.Fatalf("failed refresh=%q warnings=%v", retained, warnings)
	}
	if again, warnings := cache.load(root); again != retained || len(warnings) != 0 {
		t.Fatalf("ongoing failure repeated warning: %q %v", again, warnings)
	}
	// Recovery must reread even if the restored source has its original stat data.
	write(backup, "new-a")
	if err := os.Chtimes(backup, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if got, warnings := cache.load(root); !strings.Contains(got, "new-a") || strings.Contains(got, "old-a") || len(warnings) != 0 {
		t.Fatalf("recovered=%q warnings=%v", got, warnings)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if _, warnings := cache.load(root); len(warnings) != 1 {
		t.Fatalf("failure after recovery did not warn again: %v", warnings)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(path, " \n")
	if got, warnings := cache.load(root); got != "" || len(warnings) != 0 {
		t.Fatalf("blank source retained guidance: %q %v", got, warnings)
	}
	write(path, "remove me")
	cache.load(root)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, warnings := cache.load(root); got != "" || len(warnings) != 0 || len(cache.files) != 0 {
		t.Fatalf("deleted source retained guidance: %q %v", got, warnings)
	}
}

func TestProjectInstructionsStatFailureAndReadRace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(path, []byte("retained"), 0644); err != nil {
		t.Fatal(err)
	}
	entries := findProjectInstructions(root)
	_, previous, _ := renderProjectInstructions(entries, nil)
	entries[0].err = &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
	got, files, warnings := renderProjectInstructions(entries, previous)
	if !strings.Contains(got, "retained") || len(warnings) != 1 || files[path].warning == "" {
		t.Fatalf("stat failure=%q files=%+v warnings=%v", got, files, warnings)
	}
	if got, _, warnings := renderProjectInstructions(entries, nil); got != "" || len(warnings) != 1 {
		t.Fatalf("initial stat failure invented guidance: %q %v", got, warnings)
	}
	entries[0].err = nil
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, files, warnings := renderProjectInstructions(entries, files); got != "" || len(files) != 0 || len(warnings) != 0 {
		t.Fatalf("discovery/read deletion race retained guidance: %q %v", got, warnings)
	}
}

func TestProjectInstructionsInitialFailureIsRetried(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	var cache projectInstructionCache
	for attempt := range 2 {
		got, warnings := cache.load(root)
		if got != "" || len(warnings) != 1-attempt {
			t.Fatalf("attempt %d: invented guidance or repeated warning: %q %v", attempt, got, warnings)
		}
		if len(warnings) > 0 && strings.Contains(warnings[0], "keeping") {
			t.Fatalf("claimed to retain content that was never loaded: %v", warnings)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("recovered guidance"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, warnings := cache.load(root); !strings.Contains(got, "recovered guidance") || len(warnings) != 0 {
		t.Fatalf("failed to recover: %q %v", got, warnings)
	}
}
