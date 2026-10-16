package lsp

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

const (
	maxListedLocations     = 100
	maxDefinitionSnippets  = 5
	definitionSnippetLines = 8
)

func relativePath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return path
}

func locationPath(uri string) string {
	if path, ok := fileuri.Path(uri); ok {
		return path
	}
	return uri
}

type sourceCache map[string][]string

func (c sourceCache) lines(path string) []string {
	if lines, ok := c[path]; ok {
		return lines
	}
	// Source context is optional; never read devices or unbounded server paths.
	c[path] = nil
	const maxSourceBytes = 2 << 20
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSourceBytes {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSourceBytes+1))
	if err != nil || len(data) > maxSourceBytes {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	c[path] = lines
	return lines
}

func (c sourceCache) locationLine(location lsp.Location) (string, int, bool) {
	path, ok := fileuri.Path(location.URI.String())
	if !ok {
		return "", int(location.Range.Start.Character) + 1, false
	}
	lines := c.lines(path)
	line := int(location.Range.Start.Line)
	if line < 0 || line >= len(lines) {
		return "", int(location.Range.Start.Character) + 1, false
	}
	return lines[line], runeColumnFromUTF16(lines[line], int(location.Range.Start.Character)) + 1, true
}

func sortedLocations(locations []lsp.Location) []lsp.Location {
	locations = slices.Clone(locations)
	slices.SortStableFunc(locations, func(a, b lsp.Location) int {
		return cmp.Or(cmp.Compare(a.URI.String(), b.URI.String()), cmp.Compare(a.Range.Start.Line, b.Range.Start.Line), cmp.Compare(a.Range.Start.Character, b.Range.Start.Character), cmp.Compare(a.Range.End.Line, b.Range.End.Line), cmp.Compare(a.Range.End.Character, b.Range.End.Character))
	})
	return locations
}

func formatLocations(title string, locations []lsp.Location, root string, pages ...resultPage) string {
	p := selectedPage(pages)
	files := make(map[string]bool)
	for _, location := range locations {
		files[location.URI.String()] = true
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s (%d found across %d files):\n", title, len(locations), len(files))
	shown := pageSlice(sortedLocations(locations), p)
	cache := sourceCache{}
	for _, location := range shown {
		path := locationPath(location.URI.String())
		text, column, found := cache.locationLine(location)
		if found {
			fmt.Fprintf(&out, "%s:%d:%d: %s\n", relativePath(root, path), location.Range.Start.Line+1, column, strings.TrimSpace(text))
		} else {
			fmt.Fprintf(&out, "%s:%d:%d\n", relativePath(root, path), location.Range.Start.Line+1, column)
		}
	}
	out.WriteString(pageNote(len(locations), p, "locations"))
	return out.String()
}

func formatReferenceCounts(locations []lsp.Location, root string, includeDeclaration bool, p resultPage) string {
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, location := range locations {
		key := fmt.Sprintf("%s:%d:%d:%d:%d", location.URI.String(), location.Range.Start.Line, location.Range.Start.Character, location.Range.End.Line, location.Range.End.Character)
		if seen[key] {
			continue
		}
		seen[key] = true
		path, ok := fileuri.Path(location.URI.String())
		if !ok {
			path = location.URI.String()
		}
		counts[relativePath(root, path)]++
	}
	var files []string
	for file := range counts {
		files = append(files, file)
	}
	slices.Sort(files)
	declaration := "included"
	if !includeDeclaration {
		declaration = "excluded"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "References: %d across %d files; declaration %s. Uses include non-call references.\n", len(seen), len(files), declaration)
	for _, file := range pageSlice(files, p) {
		fmt.Fprintf(&out, "%s: %d\n", file, counts[file])
	}
	out.WriteString(pageNote(len(files), p, "files"))
	return out.String()
}

func formatDefinitions(locations []lsp.Location, root string, pages ...resultPage) string {
	p := selectedPage(pages)
	var out strings.Builder
	fmt.Fprintf(&out, "Definition (%d found):\n", len(locations))
	shown := pageSlice(sortedLocations(locations), p)
	cache := sourceCache{}
	for i, location := range shown {
		path := locationPath(location.URI.String())
		_, column, found := cache.locationLine(location)
		fmt.Fprintf(&out, "\n%s:%d:%d\n", relativePath(root, path), location.Range.Start.Line+1, column)
		if i >= maxDefinitionSnippets || !found {
			continue
		}
		lines := cache.lines(path)
		start := int(location.Range.Start.Line)
		for line := start; line < min(start+definitionSnippetLines, len(lines)); line++ {
			fmt.Fprintf(&out, "%6d\t%s\n", line+1, lines[line])
		}
	}
	out.WriteString(pageNote(len(locations), p, "locations"))
	return out.String()
}

func formatSymbolInformation(symbols []lsp.SymbolInformation, root string, pages ...resultPage) string {
	return formatSymbols(len(symbols), root, symbols, func(symbol lsp.SymbolInformation) (string, string) {
		path := locationPath(symbol.Location.URI.String())
		return path, fmt.Sprintf("  %s (%s) - Line %d\n", symbol.Name, symbolKindName(symbol.Kind), symbol.Location.Range.Start.Line+1)
	}, selectedPage(pages))
}

func formatWorkspaceSymbols(symbols []lsp.WorkspaceSymbol, root string, pages ...resultPage) string {
	return formatSymbols(len(symbols), root, symbols, func(symbol lsp.WorkspaceSymbol) (string, string) {
		switch location := symbol.Location.(type) {
		case *lsp.Location:
			path := locationPath(location.URI.String())
			return path, fmt.Sprintf("  %s (%s) - Line %d\n", symbol.Name, symbolKindName(symbol.Kind), location.Range.Start.Line+1)
		case *lsp.LocationURIOnly:
			path := locationPath(location.URI.String())
			return path, fmt.Sprintf("  %s (%s)\n", symbol.Name, symbolKindName(symbol.Kind))
		default:
			return "", fmt.Sprintf("  %s (%s)\n", symbol.Name, symbolKindName(symbol.Kind))
		}
	}, selectedPage(pages))
}

func formatSymbols[T any](total int, root string, symbols []T, line func(T) (string, string), p resultPage) string {
	symbols = slices.Clone(symbols)
	slices.SortStableFunc(symbols, func(a, b T) int {
		ap, at := line(a)
		bp, bt := line(b)
		return cmp.Or(cmp.Compare(ap, bp), cmp.Compare(at, bt))
	})
	symbols = pageSlice(symbols, p)
	type group struct {
		path  string
		lines []string
	}
	indexes := make(map[string]int)
	var groups []group
	for _, symbol := range symbols {
		path, text := line(symbol)
		path = relativePath(root, path)
		index, ok := indexes[path]
		if !ok {
			index = len(groups)
			indexes[path] = index
			groups = append(groups, group{path: path})
		}
		groups[index].lines = append(groups[index].lines, text)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Symbols (%d found; %d files on this page):\n", total, len(groups))
	for _, group := range groups {
		fmt.Fprintf(&out, "\n%s:\n", group.path)
		for _, line := range group.lines {
			out.WriteString(line)
		}
	}
	out.WriteString(pageNote(total, p, "symbols"))
	return out.String()
}

func formatIncomingCalls(calls []lsp.CallHierarchyIncomingCall, root string, pages ...resultPage) string {
	p := selectedPage(pages)
	calls = slices.Clone(calls)
	slices.SortStableFunc(calls, func(a, b lsp.CallHierarchyIncomingCall) int { return compareCallItems(a.From, b.From) })
	var out strings.Builder
	fmt.Fprintf(&out, "Incoming Calls (%d found):\n", len(calls))
	for _, call := range pageSlice(calls, p) {
		path := locationPath(call.From.URI.String())
		fmt.Fprintf(&out, "  %s (%s) - %s:%d%s\n", call.From.Name, symbolKindName(call.From.Kind), relativePath(root, path), call.From.SelectionRange.Start.Line+1, formatCallSites(call.FromRanges, ", call sites at lines "))
	}
	out.WriteString(pageNote(len(calls), p, "callers"))
	return out.String()
}

func formatOutgoingCalls(calls []lsp.CallHierarchyOutgoingCall, root string, pages ...resultPage) string {
	p := selectedPage(pages)
	calls = slices.Clone(calls)
	slices.SortStableFunc(calls, func(a, b lsp.CallHierarchyOutgoingCall) int { return compareCallItems(a.To, b.To) })
	var out strings.Builder
	fmt.Fprintf(&out, "Outgoing Calls (%d found):\n", len(calls))
	for _, call := range pageSlice(calls, p) {
		path := locationPath(call.To.URI.String())
		fmt.Fprintf(&out, "  %s (%s) - %s:%d%s\n", call.To.Name, symbolKindName(call.To.Kind), relativePath(root, path), call.To.SelectionRange.Start.Line+1, formatCallSites(call.FromRanges, ", called at lines "))
	}
	out.WriteString(pageNote(len(calls), p, "callees"))
	return out.String()
}

func compareCallItems(a, b lsp.CallHierarchyItem) int {
	return cmp.Or(cmp.Compare(a.URI.String(), b.URI.String()), cmp.Compare(a.SelectionRange.Start.Line, b.SelectionRange.Start.Line), cmp.Compare(a.SelectionRange.Start.Character, b.SelectionRange.Start.Character), cmp.Compare(a.Name, b.Name))
}

func formatDocumentSymbolPage(symbols []lsp.DocumentSymbol, path, root string, p resultPage) string {
	type entry struct {
		symbol    lsp.DocumentSymbol
		qualified string
	}
	var entries []entry
	var walk func([]lsp.DocumentSymbol, string)
	walk = func(symbols []lsp.DocumentSymbol, prefix string) {
		for _, symbol := range symbols {
			name := symbol.Name
			if prefix != "" {
				name = prefix + "." + name
			}
			entries = append(entries, entry{symbol, name})
			walk(symbol.Children, name)
		}
	}
	walk(symbols, "")
	return formatSymbols(len(entries), root, entries, func(e entry) (string, string) {
		detail := ""
		if e.symbol.Detail != nil && *e.symbol.Detail != "" {
			detail = " " + *e.symbol.Detail
		}
		return path, fmt.Sprintf("  %s (%s)%s - Line %d\n", e.qualified, symbolKindName(e.symbol.Kind), detail, e.symbol.SelectionRange.Start.Line+1)
	}, p)
}

func formatCallSites(ranges []lsp.Range, prefix string) string {
	seen := make(map[uint32]bool)
	var lines []string
	for _, value := range ranges {
		line := value.Start.Line + 1
		if !seen[line] {
			seen[line] = true
			lines = append(lines, fmt.Sprint(line))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return prefix + strings.Join(lines, ", ")
}

func formatWorkspaceDiagnostics(report language.WorkspaceReport, root string) string {
	type fileDiagnostic struct {
		path       string
		diagnostic language.Diagnostic
	}
	var all []fileDiagnostic
	for path, values := range report.Diagnostics {
		for _, value := range values {
			all = append(all, fileDiagnostic{relativePath(root, path), value})
		}
	}
	slices.SortStableFunc(all, func(a, b fileDiagnostic) int {
		return language.CompareSeverity(a.diagnostic.Severity, b.diagnostic.Severity)
	})
	var notes []string
	if report.DiscoveredFiles > report.CheckedFiles || report.DiscoveryTruncated {
		total := fmt.Sprint(report.DiscoveredFiles)
		if report.DiscoveryTruncated {
			total += "+"
		}
		notes = append(notes, fmt.Sprintf("checked %d of %s source files; results are partial", report.CheckedFiles, total))
	} else {
		notes = append(notes, fmt.Sprintf("checked %d source files", report.CheckedFiles))
	}
	if len(report.UnavailableServers) > 0 {
		notes = append(notes, "server unavailable: "+strings.Join(report.UnavailableServers, ", "))
	}
	if report.Analyzing {
		notes = append(notes, "a language server is still analyzing; rerun for final results")
	}
	coverage := "Coverage: " + strings.Join(notes, "; ")
	if len(all) == 0 {
		return "No workspace diagnostics found\n" + coverage
	}
	diagnosticValues := make([]language.Diagnostic, len(all))
	for i := range all {
		diagnosticValues[i] = all[i].diagnostic
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Workspace Diagnostics (%d found: %s):\n", len(all), language.DiagnosticSummary(diagnosticValues))
	for _, value := range all {
		fmt.Fprintf(&out, "  %s\n", language.FormatDiagnosticLine(value.path, value.diagnostic))
	}
	out.WriteString(coverage)
	return out.String()
}

var symbolKindNames = map[lsp.SymbolKind]string{
	lsp.SymbolKindFile: "File", lsp.SymbolKindModule: "Module",
	lsp.SymbolKindNamespace: "Namespace", lsp.SymbolKindPackage: "Package",
	lsp.SymbolKindClass: "Class", lsp.SymbolKindMethod: "Method",
	lsp.SymbolKindProperty: "Property", lsp.SymbolKindField: "Field",
	lsp.SymbolKindConstructor: "Constructor", lsp.SymbolKindEnum: "Enum",
	lsp.SymbolKindInterface: "Interface", lsp.SymbolKindFunction: "Function",
	lsp.SymbolKindVariable: "Variable", lsp.SymbolKindConstant: "Constant",
	lsp.SymbolKindString: "String", lsp.SymbolKindNumber: "Number",
	lsp.SymbolKindBoolean: "Boolean", lsp.SymbolKindArray: "Array",
	lsp.SymbolKindObject: "Object", lsp.SymbolKindKey: "Key",
	lsp.SymbolKindNull: "Null", lsp.SymbolKindEnumMember: "EnumMember",
	lsp.SymbolKindStruct: "Struct", lsp.SymbolKindEvent: "Event",
	lsp.SymbolKindOperator: "Operator", lsp.SymbolKindTypeParameter: "TypeParameter",
}

func symbolKindName(kind lsp.SymbolKind) string {
	if name := symbolKindNames[kind]; name != "" {
		return name
	}
	return "Symbol"
}
