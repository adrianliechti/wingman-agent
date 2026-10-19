package code

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestDiagnosticsFooterFollowsBackgroundUpdates(t *testing.T) {
	a, _ := newStreamTestApp(nil)
	a.operation = operationDiagnostics
	for _, progress := range []string{"", "Installing gopls", "Starting gopls", ""} {
		a.dirty = false
		a.SetBackgroundStatus(progress, false)
		drainStateUpdates(a)
		if !a.dirty {
			t.Fatal("progress update did not request a render")
		}
		want := "Scanning workspace problems"
		if progress != "" {
			want += " · " + progress
		}
		want += " · Esc cancels"
		if footer := strings.TrimSpace(ansi.Strip(a.footerLine(100))); footer != want {
			t.Fatalf("footer = %q, want %q", footer, want)
		}
	}

	// A queued progress update must not relabel the next operation.
	a.SetBackgroundStatus("Starting gopls", false)
	a.operation = operationCompact
	drainStateUpdates(a)
	if footer := strings.TrimSpace(ansi.Strip(a.footerLine(100))); footer != "Compacting context · Esc cancels" {
		t.Fatalf("late progress replaced compaction status: %q", footer)
	}
}

func TestDiagnosticsCoverageComplete(t *testing.T) {
	coverage, partial := diagnosticsCoverage(language.WorkspaceReport{
		CheckedFiles:    3,
		DiscoveredFiles: 3,
	})
	if partial {
		t.Fatal("complete coverage reported as partial")
	}
	if coverage != "checked 3/3 files" {
		t.Fatalf("coverage = %q", coverage)
	}
}

func TestDiagnosticsCoverageExplainsPartialResults(t *testing.T) {
	coverage, partial := diagnosticsCoverage(language.WorkspaceReport{
		CheckedFiles:       4,
		DiscoveredFiles:    10,
		DiscoveryTruncated: true,
		UnknownFiles:       2,
		UnavailableServers: []string{"gopls (tools)"},
	})
	if !partial {
		t.Fatal("incomplete coverage was not reported as partial")
	}
	for _, want := range []string{"checked 4/10+ files", "2 unknown", "unavailable: gopls (tools)"} {
		if !strings.Contains(coverage, want) {
			t.Fatalf("coverage %q does not contain %q", coverage, want)
		}
	}
}
