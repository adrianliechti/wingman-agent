package language

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

const (
	editDiagnosticsWait = 2 * time.Second
	editDiagnosticsShow = 10
)

// FileDiagnostics returns an open document's diagnostics. The boolean is
// false when the server has not reported any yet.
func (s *Service) FileDiagnostics(ctx context.Context, filePath string, content *string) ([]lsp.Diagnostic, bool, error) {
	var known bool
	raw, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.Diagnostic, error) {
		diagnostics, reported := session.WaitForDiagnostics(ctx, uri, 2*time.Second)
		known = reported
		return diagnostics, nil
	})
	return raw, known, err
}

// EditDiagnostics returns errors and warnings for the text written by the edit.
func (s *Service) EditDiagnostics(ctx context.Context, filePath, editedText string) string {
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return ""
	}
	if server := s.manager.FindServer(filePath); server == nil || !server.ReportsDiagnostics() {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, editDiagnosticsWait)
	defer cancel()
	session, err := s.manager.GetSession(ctx, filePath)
	if err != nil {
		return ""
	}
	report := session.DiagnosticsForEdit(ctx, filePath, editedText, editDiagnosticsWait)
	var errors, warnings []lsp.Diagnostic
	for _, value := range report {
		switch value.Severity {
		case lsp.DiagnosticSeverityError, 0:
			errors = append(errors, value)
		case lsp.DiagnosticSeverityWarning:
			warnings = append(warnings, value)
		}
	}
	found := slices.Concat(errors, warnings)
	if len(found) == 0 {
		return ""
	}
	path := filePath
	if rel, err := filepath.Rel(s.root, filePath); err == nil && filepath.IsLocal(rel) {
		path = rel
	}
	var out strings.Builder
	fmt.Fprintf(&out, "lsp diagnostics for %s (%d errors, %d warnings):", path, len(errors), len(warnings))
	for _, value := range found[:min(len(found), editDiagnosticsShow)] {
		severity := "Error"
		if value.Severity == lsp.DiagnosticSeverityWarning {
			severity = "Warning"
		}
		fmt.Fprintf(&out, "\n  %s:%d:%d %s: ", path, int64(value.Range.Start.Line)+1, int64(value.Range.Start.Character)+1, severity)
		if source, _ := value.Source.Get(); source != "" {
			fmt.Fprintf(&out, "[%s] ", source)
		}
		switch message := value.Message.(type) {
		case lsp.String:
			out.WriteString(string(message))
		case *lsp.MarkupContent:
			out.WriteString(message.Value)
		}
	}
	if len(found) > editDiagnosticsShow {
		fmt.Fprintf(&out, "\n  ... and %d more", len(found)-editDiagnosticsShow)
	}
	return out.String()
}
