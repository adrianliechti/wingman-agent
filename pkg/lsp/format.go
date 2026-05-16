package lsp

import (
	"fmt"
	"path/filepath"
	"strings"
)

func relPath(workingDir, path string) string {
	if rel, err := filepath.Rel(workingDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func formatLocations(title string, locations []Location, workingDir string) string {
	var sb strings.Builder
	files := groupLocationsByFile(locations, workingDir)
	fmt.Fprintf(&sb, "%s (%d found across %d files):\n", title, len(locations), len(files))

	for _, file := range files {
		fmt.Fprintf(&sb, "\n%s:\n", file.Path)
		for _, loc := range file.Locations {
			fmt.Fprintf(&sb, "  Line %d:%d\n", loc.Range.Start.Line+1, loc.Range.Start.Character+1)
		}
	}

	return sb.String()
}

type locationsByFile struct {
	Path      string
	Locations []Location
}

func groupLocationsByFile(locations []Location, workingDir string) []locationsByFile {
	indexes := map[string]int{}
	files := []locationsByFile{}

	for _, loc := range locations {
		path := relPath(workingDir, uriToPath(loc.URI))
		idx, ok := indexes[path]
		if !ok {
			idx = len(files)
			indexes[path] = idx
			files = append(files, locationsByFile{Path: path})
		}

		files[idx].Locations = append(files[idx].Locations, loc)
	}

	return files
}

func formatDocumentSymbols(symbols []DocumentSymbol, filePath string, workingDir string, indent int) string {
	var sb strings.Builder

	if indent == 0 {
		fmt.Fprintf(&sb, "Symbols in %s:\n", relPath(workingDir, filePath))
	}

	prefix := strings.Repeat("  ", indent+1)

	for _, sym := range symbols {
		detail := ""
		if sym.Detail != "" {
			detail = " " + sym.Detail
		}
		fmt.Fprintf(&sb, "%s%s (%s)%s - line %d\n", prefix, sym.Name, symbolKindName(sym.Kind), detail, sym.SelectionRange.Start.Line+1)

		if len(sym.Children) > 0 {
			fmt.Fprint(&sb, formatDocumentSymbols(sym.Children, filePath, workingDir, indent+1))
		}
	}

	return sb.String()
}

func formatSymbolInformations(symbols []SymbolInformation, workingDir string) string {
	var sb strings.Builder
	files := groupSymbolInformationsByFile(symbols, workingDir)
	fmt.Fprintf(&sb, "Symbols (%d found across %d files):\n", len(symbols), len(files))

	for _, file := range files {
		fmt.Fprintf(&sb, "\n%s:\n", file.Path)
		for _, sym := range file.Symbols {
			fmt.Fprintf(&sb, "  %s (%s) - Line %d\n", sym.Name, symbolKindName(sym.Kind), sym.Location.Range.Start.Line+1)
		}
	}

	return sb.String()
}

type symbolInformationsByFile struct {
	Path    string
	Symbols []SymbolInformation
}

func groupSymbolInformationsByFile(symbols []SymbolInformation, workingDir string) []symbolInformationsByFile {
	indexes := map[string]int{}
	files := []symbolInformationsByFile{}

	for _, sym := range symbols {
		path := relPath(workingDir, uriToPath(sym.Location.URI))
		idx, ok := indexes[path]
		if !ok {
			idx = len(files)
			indexes[path] = idx
			files = append(files, symbolInformationsByFile{Path: path})
		}

		files[idx].Symbols = append(files[idx].Symbols, sym)
	}

	return files
}

func formatWorkspaceSymbols(symbols []WorkspaceSymbol, workingDir string) string {
	var sb strings.Builder
	files := groupWorkspaceSymbolsByFile(symbols, workingDir)
	fmt.Fprintf(&sb, "Symbols (%d found across %d files):\n", len(symbols), len(files))

	for _, file := range files {
		fmt.Fprintf(&sb, "\n%s:\n", file.Path)
		for _, sym := range file.Symbols {
			if sym.Location.Range != nil {
				fmt.Fprintf(&sb, "  %s (%s) - Line %d\n", sym.Name, symbolKindName(sym.Kind), sym.Location.Range.Start.Line+1)
			} else {
				fmt.Fprintf(&sb, "  %s (%s)\n", sym.Name, symbolKindName(sym.Kind))
			}
		}
	}

	return sb.String()
}

type workspaceSymbolsByFile struct {
	Path    string
	Symbols []WorkspaceSymbol
}

func groupWorkspaceSymbolsByFile(symbols []WorkspaceSymbol, workingDir string) []workspaceSymbolsByFile {
	indexes := map[string]int{}
	files := []workspaceSymbolsByFile{}

	for _, sym := range symbols {
		path := relPath(workingDir, uriToPath(sym.Location.URI))
		idx, ok := indexes[path]
		if !ok {
			idx = len(files)
			indexes[path] = idx
			files = append(files, workspaceSymbolsByFile{Path: path})
		}

		files[idx].Symbols = append(files[idx].Symbols, sym)
	}

	return files
}

func formatCallHierarchyItems(items []CallHierarchyItem, workingDir string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy items (%d found):\n", len(items))

	for _, item := range items {
		path := relPath(workingDir, uriToPath(item.URI))
		detail := ""
		if item.Detail != "" {
			detail = " [" + item.Detail + "]"
		}
		fmt.Fprintf(&sb, "  %s (%s) - %s:%d%s\n", item.Name, symbolKindName(item.Kind), path, item.SelectionRange.Start.Line+1, detail)
	}

	return sb.String()
}

func formatIncomingCalls(calls []CallHierarchyIncomingCall, workingDir string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Incoming Calls (%d found):\n", len(calls))

	for _, c := range calls {
		path := relPath(workingDir, uriToPath(c.From.URI))
		fmt.Fprintf(&sb, "  %s (%s) - %s:%d\n", c.From.Name, symbolKindName(c.From.Kind), path, c.From.SelectionRange.Start.Line+1)
	}

	return sb.String()
}

func formatOutgoingCalls(calls []CallHierarchyOutgoingCall, workingDir string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Outgoing Calls (%d found):\n", len(calls))

	for _, c := range calls {
		path := relPath(workingDir, uriToPath(c.To.URI))
		fmt.Fprintf(&sb, "  %s (%s) - %s:%d\n", c.To.Name, symbolKindName(c.To.Kind), path, c.To.SelectionRange.Start.Line+1)
	}

	return sb.String()
}

var symbolKindNames = [...]string{
	1:  "File",
	2:  "Module",
	3:  "Namespace",
	4:  "Package",
	5:  "Class",
	6:  "Method",
	7:  "Property",
	8:  "Field",
	9:  "Constructor",
	10: "Enum",
	11: "Interface",
	12: "Function",
	13: "Variable",
	14: "Constant",
	15: "String",
	16: "Number",
	17: "Boolean",
	18: "Array",
	19: "Object",
	20: "Key",
	21: "Null",
	22: "EnumMember",
	23: "Struct",
	24: "Event",
	25: "Operator",
	26: "TypeParameter",
}

func symbolKindName(kind int) string {
	if kind >= 1 && kind < len(symbolKindNames) && symbolKindNames[kind] != "" {
		return symbolKindNames[kind]
	}
	return "Symbol"
}

func FormatDiagnostics(diagnostics []Diagnostic, filePath string, workingDir string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Diagnostics (%d found):\n", len(diagnostics))

	displayPath := relPath(workingDir, filePath)

	for _, diag := range diagnostics {
		source := ""
		if diag.Source != "" {
			source = fmt.Sprintf("[%s] ", diag.Source)
		}
		fmt.Fprintf(&sb, "  %s:%d:%d %s: %s%s\n", displayPath, diag.Range.Start.Line+1, diag.Range.Start.Character+1, DiagnosticSeverityName(diag.Severity), source, diag.Message)
	}

	return sb.String()
}

func DiagnosticSeverityName(severity int) string {
	switch severity {
	case DiagnosticSeverityError:
		return "Error"
	case DiagnosticSeverityWarning:
		return "Warning"
	case DiagnosticSeverityInformation:
		return "Info"
	case DiagnosticSeverityHint:
		return "Hint"
	default:
		return "Unknown"
	}
}
