package lsp

import (
	"strings"
	"testing"
)

func TestFormatDiagnostics(t *testing.T) {
	diags := []Diagnostic{
		{
			Range:    Range{Start: Position{Line: 4, Character: 10}},
			Severity: DiagnosticSeverityError,
			Source:   "compiler",
			Message:  "undefined: bar",
		},
		{
			Range:    Range{Start: Position{Line: 12, Character: 0}},
			Severity: DiagnosticSeverityWarning,
			Message:  "unused variable",
		},
	}

	result := FormatDiagnostics(diags, "/home/user/project/main.go", "/home/user/project")

	if !strings.Contains(result, "Diagnostics (2 found)") {
		t.Error("expected header with count")
	}
	if !strings.Contains(result, "main.go:5:11") {
		t.Error("expected 1-based line:col for first diagnostic")
	}
	if !strings.Contains(result, "Error") {
		t.Error("expected Error severity")
	}
	if !strings.Contains(result, "[compiler]") {
		t.Error("expected source tag")
	}
	if !strings.Contains(result, "main.go:13:1") {
		t.Error("expected 1-based line:col for second diagnostic")
	}
	if !strings.Contains(result, "Warning") {
		t.Error("expected Warning severity")
	}
}

func TestFormatLocationsGroupsByFile(t *testing.T) {
	locations := []Location{
		{URI: FileURI("/home/user/project/a.go"), Range: Range{Start: Position{Line: 1, Character: 2}}},
		{URI: FileURI("/home/user/project/a.go"), Range: Range{Start: Position{Line: 4, Character: 0}}},
		{URI: FileURI("/home/user/project/b.go"), Range: Range{Start: Position{Line: 9, Character: 3}}},
	}

	result := formatLocations("References", locations, "/home/user/project")

	for _, want := range []string{
		"References (3 found across 2 files)",
		"a.go:\n  Line 2:3\n  Line 5:1",
		"b.go:\n  Line 10:4",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("formatLocations missing %q in:\n%s", want, result)
		}
	}
}

func TestFormatSymbolInformationsGroupsByFile(t *testing.T) {
	symbols := []SymbolInformation{
		{Name: "First", Kind: 12, Location: Location{URI: FileURI("/home/user/project/a.go"), Range: Range{Start: Position{Line: 3}}}},
		{Name: "Second", Kind: 6, Location: Location{URI: FileURI("/home/user/project/a.go"), Range: Range{Start: Position{Line: 8}}}},
		{Name: "Third", Kind: 11, Location: Location{URI: FileURI("/home/user/project/b.go"), Range: Range{Start: Position{Line: 13}}}},
	}

	result := formatSymbolInformations(symbols, "/home/user/project")

	for _, want := range []string{
		"Symbols (3 found across 2 files)",
		"a.go:\n  First (Function) - Line 4\n  Second (Method) - Line 9",
		"b.go:\n  Third (Interface) - Line 14",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("formatSymbolInformations missing %q in:\n%s", want, result)
		}
	}
}

func TestFormatCallHierarchyItems(t *testing.T) {
	items := []CallHierarchyItem{
		{
			Name:           "Handle",
			Kind:           12,
			Detail:         "func(ctx context.Context)",
			URI:            FileURI("/home/user/project/handler.go"),
			SelectionRange: Range{Start: Position{Line: 20}},
		},
	}

	result := formatCallHierarchyItems(items, "/home/user/project")

	for _, want := range []string{
		"Call hierarchy items (1 found)",
		"Handle (Function) - handler.go:21 [func(ctx context.Context)]",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("formatCallHierarchyItems missing %q in:\n%s", want, result)
		}
	}
}

func TestDiagnosticSeverityName(t *testing.T) {
	tests := []struct {
		severity int
		want     string
	}{
		{DiagnosticSeverityError, "Error"},
		{DiagnosticSeverityWarning, "Warning"},
		{DiagnosticSeverityInformation, "Info"},
		{DiagnosticSeverityHint, "Hint"},
		{0, "Unknown"},
		{99, "Unknown"},
	}

	for _, tt := range tests {
		got := DiagnosticSeverityName(tt.severity)
		if got != tt.want {
			t.Errorf("DiagnosticSeverityName(%d) = %q, want %q", tt.severity, got, tt.want)
		}
	}
}
