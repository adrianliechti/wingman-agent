package code

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestDiagnosticsScanningStatusShowsInFooter(t *testing.T) {
	a := &App{
		operationPending: true,
		operationStatus:  "Scanning workspace problems · Esc cancels",
	}
	footer := ansi.Strip(a.footerLine(80))
	if !strings.Contains(footer, "Scanning workspace problems · Esc cancels") {
		t.Fatalf("footer = %q, want scanning status", footer)
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
