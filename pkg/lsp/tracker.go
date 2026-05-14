package lsp

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

const maxDiagnosticsPerFile = 10

type DiagnosticTracker struct {
	baselines map[string]map[string]struct{}
	delivered map[string]map[string]struct{}

	mu sync.Mutex
}

func NewDiagnosticTracker() *DiagnosticTracker {
	return &DiagnosticTracker{
		baselines: make(map[string]map[string]struct{}),
		delivered: make(map[string]map[string]struct{}),
	}
}

func (t *DiagnosticTracker) SetBaseline(uri string, diags []Diagnostic) {
	keys := make(map[string]struct{}, len(diags))
	for _, d := range diags {
		keys[diagnosticKey(d)] = struct{}{}
	}

	t.mu.Lock()
	t.baselines[uri] = keys
	delete(t.delivered, uri)
	t.mu.Unlock()
}

func (t *DiagnosticTracker) FilterNew(uri string, diags []Diagnostic) []Diagnostic {
	t.mu.Lock()
	baseline := t.baselines[uri]
	deliveredSet := t.delivered[uri]
	t.mu.Unlock()

	var filtered []Diagnostic
	for _, d := range diags {
		key := diagnosticKey(d)

		if baseline != nil {
			if _, inBaseline := baseline[key]; inBaseline {
				continue
			}
		}

		if deliveredSet != nil {
			if _, wasDelivered := deliveredSet[key]; wasDelivered {
				continue
			}
		}

		filtered = append(filtered, d)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Severity < filtered[j].Severity
	})

	if len(filtered) > maxDiagnosticsPerFile {
		filtered = filtered[:maxDiagnosticsPerFile]
	}

	return filtered
}

func (t *DiagnosticTracker) MarkDelivered(uri string, diags []Diagnostic) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.delivered[uri] == nil {
		t.delivered[uri] = make(map[string]struct{})
	}

	for _, d := range diags {
		t.delivered[uri][diagnosticKey(d)] = struct{}{}
	}
}

func diagnosticKey(d Diagnostic) string {
	return fmt.Sprintf("%d:%d:%d:%s", d.Range.Start.Line, d.Range.Start.Character, d.Severity, d.Message)
}

func FormatNewDiagnostics(diagnostics []Diagnostic, filePath string, workingDir string) string {
	if len(diagnostics) == 0 {
		return ""
	}

	var sb strings.Builder

	displayPath := relPath(workingDir, filePath)

	fmt.Fprintf(&sb, "%s:\n", displayPath)
	for _, diag := range diagnostics {
		symbol := severitySymbol(diag.Severity)
		code := ""
		if diag.Code != nil {
			code = fmt.Sprintf(" [%v]", diag.Code)
		}
		source := ""
		if diag.Source != "" {
			source = fmt.Sprintf(" (%s)", diag.Source)
		}
		fmt.Fprintf(&sb, "  %s [Line %d:%d] %s%s%s\n",
			symbol,
			diag.Range.Start.Line+1,
			diag.Range.Start.Character+1,
			diag.Message,
			code,
			source,
		)
	}

	return sb.String()
}

func severitySymbol(severity int) string {
	switch severity {
	case DiagnosticSeverityError:
		return "✘"
	case DiagnosticSeverityWarning:
		return "⚠"
	case DiagnosticSeverityInformation:
		return "ℹ"
	case DiagnosticSeverityHint:
		return "★"
	default:
		return "•"
	}
}
