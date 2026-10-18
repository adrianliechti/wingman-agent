package language

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/treesitter"
)

// The TreeSitter* methods answer from syntax alone, for files without a
// language server. Cross-file results are name-matched candidates.

const maxTreeSitterSymbols = 1000

func TreeSitterSupported(filePath string) bool {
	return treesitter.Supported(filePath)
}

func (s *Service) TreeSitterDefinitions(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.Location, error) {
	relative, source, err := s.treeSitterInput(filePath, content)
	if err != nil {
		return nil, err
	}
	defs, err := s.syntax.Definitions(ctx, relative, source, line, column)
	if err != nil {
		return nil, err
	}
	return s.definitionLocations(defs)
}

func (s *Service) TreeSitterReferences(ctx context.Context, filePath string, content *string, line, column int, includeDeclaration bool) ([]lsp.Location, bool, error) {
	relative, source, err := s.treeSitterInput(filePath, content)
	if err != nil {
		return nil, false, err
	}
	values, truncated, err := s.syntax.References(ctx, relative, source, line, column, includeDeclaration)
	if err != nil {
		return nil, false, err
	}
	result := make([]lsp.Location, 0, len(values))
	for _, value := range values {
		location, err := s.location(value.File, treesitter.SymRange{StartLine: value.Line, StartCol: value.Col, EndLine: value.Line, EndCol: value.Col})
		if err != nil {
			return nil, false, err
		}
		result = append(result, location)
	}
	return result, truncated, nil
}

func (s *Service) TreeSitterImplementations(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.Location, error) {
	relative, source, err := s.treeSitterInput(filePath, content)
	if err != nil {
		return nil, err
	}
	defs, err := s.syntax.Implementations(ctx, relative, source, line, column)
	if err != nil {
		return nil, err
	}
	return s.definitionLocations(defs)
}

func (s *Service) TreeSitterHover(ctx context.Context, filePath string, content *string, line, column int) (string, error) {
	relative, source, err := s.treeSitterInput(filePath, content)
	if err != nil {
		return "", err
	}
	info, err := s.syntax.Hover(ctx, relative, source, line, column)
	if err != nil || info == nil {
		return "", err
	}
	code := info.Code
	if info.Truncated {
		code += "\n…"
	}
	def := info.Definition
	var result strings.Builder
	fmt.Fprintf(&result, "```%s\n%s\n```\n\n%s · %s:%d", def.Lang, code, def.Kind, def.File, def.NameRange.StartLine+1)
	if info.Others > 0 {
		fmt.Fprintf(&result, " · %d more candidates", info.Others)
	}
	return result.String(), nil
}

func (s *Service) TreeSitterDocumentSymbols(filePath string, content *string) ([]lsp.DocumentSymbol, error) {
	source, err := editorSource(filePath, content)
	if err != nil {
		return nil, err
	}
	return documentSymbols(treesitter.FileSymbols(filePath, source)), nil
}

// TreeSitterWorkspaceSymbols searches every supported file, or with
// uncoveredOnly only those no language server is configured for. The bool
// reports a result capped at maxTreeSitterSymbols.
func (s *Service) TreeSitterWorkspaceSymbols(ctx context.Context, query string, uncoveredOnly bool) ([]lsp.SymbolInformation, bool, error) {
	var include func(string) bool
	if uncoveredOnly {
		include = func(file string) bool {
			return !s.hasLSPServerFor(filepath.Join(s.root, filepath.FromSlash(file)))
		}
	}
	defs, truncated, err := s.syntax.Symbols(ctx, query, maxTreeSitterSymbols, include)
	if err != nil {
		return nil, false, err
	}
	result := make([]lsp.SymbolInformation, 0, len(defs))
	for _, def := range defs {
		location, err := s.location(def.File, def.NameRange)
		if err != nil {
			return nil, false, err
		}
		var symbol lsp.SymbolInformation
		symbol.Name, symbol.Kind, symbol.Location = def.Name, lsp.SymbolKind(symbolKind(def.Kind)), location
		result = append(result, symbol)
	}
	return result, truncated, nil
}

func (s *Service) TreeSitterIncomingCalls(ctx context.Context, filePath string, line, column int) ([]lsp.CallHierarchyIncomingCall, error) {
	relative, source, err := s.treeSitterInput(filePath, nil)
	if err != nil {
		return nil, err
	}
	calls, err := s.syntax.IncomingCalls(ctx, relative, source, line, column)
	if err != nil {
		return nil, err
	}
	result := make([]lsp.CallHierarchyIncomingCall, 0, len(calls))
	for _, call := range calls {
		item, err := s.callItem(call.Definition)
		if err != nil {
			return nil, err
		}
		result = append(result, lsp.CallHierarchyIncomingCall{From: item, FromRanges: protocolRanges(call.Sites)})
	}
	return result, nil
}

// TreeSitterOutgoingCalls also returns callee names without a workspace
// declaration, typically library or dynamic calls.
func (s *Service) TreeSitterOutgoingCalls(ctx context.Context, filePath string, line, column int) ([]lsp.CallHierarchyOutgoingCall, []string, error) {
	relative, source, err := s.treeSitterInput(filePath, nil)
	if err != nil {
		return nil, nil, err
	}
	calls, err := s.syntax.OutgoingCalls(ctx, relative, source, line, column)
	if err != nil {
		return nil, nil, err
	}
	var result []lsp.CallHierarchyOutgoingCall
	var unresolved []string
	for _, call := range calls {
		if call.File == "" {
			unresolved = append(unresolved, call.Name)
			continue
		}
		item, err := s.callItem(call.Definition)
		if err != nil {
			return nil, nil, err
		}
		result = append(result, lsp.CallHierarchyOutgoingCall{To: item, FromRanges: protocolRanges(call.Sites)})
	}
	return result, unresolved, nil
}

func (s *Service) treeSitterInput(filePath string, content *string) (string, []byte, error) {
	relative, err := filepath.Rel(s.root, filePath)
	if err != nil {
		return "", nil, err
	}
	var source []byte
	if content != nil {
		source = []byte(*content)
	}
	return filepath.ToSlash(relative), source, nil
}

func (s *Service) location(file string, value treesitter.SymRange) (lsp.Location, error) {
	uri, err := lsp.ParseURI(fileuri.FromPath(filepath.Join(s.root, filepath.FromSlash(file))))
	if err != nil {
		return lsp.Location{}, err
	}
	return lsp.Location{URI: uri, Range: protocolRange(value)}, nil
}

func (s *Service) definitionLocations(defs []treesitter.Definition) ([]lsp.Location, error) {
	result := make([]lsp.Location, 0, len(defs))
	for _, def := range defs {
		location, err := s.location(def.File, def.NameRange)
		if err != nil {
			return nil, err
		}
		result = append(result, location)
	}
	return result, nil
}

func (s *Service) callItem(def treesitter.Definition) (lsp.CallHierarchyItem, error) {
	location, err := s.location(def.File, def.Range)
	if err != nil {
		return lsp.CallHierarchyItem{}, err
	}
	return lsp.CallHierarchyItem{
		Name:           def.Name,
		Kind:           lsp.SymbolKind(symbolKind(def.Kind)),
		URI:            location.URI,
		Range:          location.Range,
		SelectionRange: protocolRange(def.NameRange),
	}, nil
}

func (s *Service) treeSitterCompletionItems(filePath string, content *string) ([]lsp.CompletionItem, error) {
	source, err := editorSource(filePath, content)
	if err != nil {
		return nil, err
	}
	result := make([]lsp.CompletionItem, 0, 128)
	seen := make(map[string]bool)
	add := func(symbols []*treesitter.Symbol, origin string) {
		var walk func([]*treesitter.Symbol)
		walk = func(symbols []*treesitter.Symbol) {
			for _, symbol := range symbols {
				if symbol.Name != "" && !seen[symbol.Name] {
					seen[symbol.Name] = true
					result = append(result, lsp.CompletionItem{
						Label:  symbol.Name,
						Kind:   lsp.CompletionItemKind(completionKind(symbol.Kind)),
						Detail: lsp.Optional(string(symbol.Kind) + " · " + origin + " · tree-sitter"),
					})
				}
				walk(symbol.Children)
			}
		}
		walk(symbols)
	}
	add(treesitter.FileSymbols(filePath, source), "current file")

	// Same-language siblings are usually in scope (Go packages, local
	// modules) and cheap to parse, unlike a workspace scan per keystroke.
	const maxSiblings = 50
	dir, ext := filepath.Dir(filePath), filepath.Ext(filePath)
	entries, _ := os.ReadDir(dir)
	parsed := 0
	for _, entry := range entries {
		sibling := filepath.Join(dir, entry.Name())
		if parsed >= maxSiblings || ext == "" || filepath.Ext(sibling) != ext || sibling == filePath || !entry.Type().IsRegular() {
			continue
		}
		if info, err := entry.Info(); err != nil || info.Size() > 1<<20 {
			continue
		}
		if data, err := os.ReadFile(sibling); err == nil {
			parsed++
			add(treesitter.FileSymbols(sibling, data), entry.Name())
		}
	}
	return result, nil
}

func (s *Service) treeSitterDocumentHighlights(filePath string, content *string, line, column int) ([]lsp.DocumentHighlight, error) {
	source, err := editorSource(filePath, content)
	if err != nil {
		return nil, err
	}
	ranges := treesitter.DocumentHighlights(filePath, source, line, column)
	result := make([]lsp.DocumentHighlight, 0, len(ranges))
	for _, value := range ranges {
		result = append(result, lsp.DocumentHighlight{Range: protocolRange(value), Kind: lsp.DocumentHighlightKindText})
	}
	return result, nil
}

func (s *Service) treeSitterFoldingRanges(filePath string, content *string) ([]lsp.FoldingRange, error) {
	source, err := editorSource(filePath, content)
	if err != nil {
		return nil, err
	}
	ranges := treesitter.FoldingRanges(filePath, source)
	result := make([]lsp.FoldingRange, 0, len(ranges))
	for _, value := range ranges {
		start, end := uint32(value.StartCol), uint32(value.EndCol)
		result = append(result, lsp.FoldingRange{
			StartLine:      uint32(value.StartLine),
			StartCharacter: &start,
			EndLine:        uint32(value.EndLine),
			EndCharacter:   &end,
		})
	}
	return result, nil
}

func (s *Service) treeSitterSemanticTokens(filePath string, content *string) ([]SemanticToken, error) {
	source, err := editorSource(filePath, content)
	if err != nil {
		return nil, err
	}
	values := treesitter.SemanticTokens(filePath, source)
	result := make([]SemanticToken, 0, len(values))
	for _, value := range values {
		result = append(result, SemanticToken{
			Line:      value.Range.StartLine,
			Character: value.Range.StartCol,
			Length:    value.Range.EndCol - value.Range.StartCol,
			Type:      value.Type,
			Modifiers: value.Modifiers,
		})
	}
	return result, nil
}

func editorSource(filePath string, content *string) ([]byte, error) {
	if content != nil {
		return []byte(*content), nil
	}
	return os.ReadFile(filePath)
}

func documentSymbols(symbols []*treesitter.Symbol) []lsp.DocumentSymbol {
	result := make([]lsp.DocumentSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		result = append(result, lsp.DocumentSymbol{
			Name:           symbol.Name,
			Kind:           lsp.SymbolKind(symbolKind(symbol.Kind)),
			Range:          protocolRange(symbol.Range),
			SelectionRange: protocolRange(symbol.NameRange),
			Children:       documentSymbols(symbol.Children),
		})
	}
	return result
}

func protocolRange(value treesitter.SymRange) lsp.Range {
	return lsp.Range{
		Start: lsp.Position{Line: uint32(value.StartLine), Character: uint32(value.StartCol)},
		End:   lsp.Position{Line: uint32(value.EndLine), Character: uint32(value.EndCol)},
	}
}

func protocolRanges(values []treesitter.SymRange) []lsp.Range {
	result := make([]lsp.Range, 0, len(values))
	for _, value := range values {
		result = append(result, protocolRange(value))
	}
	return result
}

func completionKind(kind treesitter.Kind) int {
	switch kind {
	case treesitter.KindModule:
		return 9
	case treesitter.KindClass:
		return 7
	case treesitter.KindMethod:
		return 2
	case treesitter.KindConstructor:
		return 4
	case treesitter.KindInterface:
		return 8
	case treesitter.KindFunction:
		return 3
	case treesitter.KindConstant:
		return 21
	case treesitter.KindType:
		return 22
	default:
		return 6
	}
}

func symbolKind(kind treesitter.Kind) int {
	switch kind {
	case treesitter.KindModule:
		return 2
	case treesitter.KindClass:
		return 5
	case treesitter.KindMethod:
		return 6
	case treesitter.KindConstructor:
		return 9
	case treesitter.KindInterface:
		return 11
	case treesitter.KindFunction:
		return 12
	case treesitter.KindConstant:
		return 14
	case treesitter.KindType:
		return 23
	default:
		return 13
	}
}
