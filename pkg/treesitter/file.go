package treesitter

import (
	"cmp"
	"slices"
	"strings"

	"github.com/odvcencio/gotreesitter"
)

type SymRange struct {
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
}

type Symbol struct {
	Name      string
	Kind      Kind
	Range     SymRange
	NameRange SymRange
	Children  []*Symbol
}

// FileSymbols extracts a nested outline from a single buffer, so unsaved
// editor content stays accurate.
func FileSymbols(filename string, src []byte) []*Symbol {
	entry := detect(filename)
	if entry == nil || len(src) == 0 {
		return nil
	}

	ex := extractors.Get().(*extractor)
	defer extractors.Put(ex)

	if ex.tagger(entry) == nil {
		return nil
	}
	file := ex.parse(entry, src, true)
	if file == nil {
		return nil
	}
	defer file.release()

	type span struct {
		sym        *Symbol
		start, end uint32
	}
	var spans []span
	for _, d := range file.definitions("") {
		spans = append(spans, span{
			sym:   &Symbol{Name: d.Name, Kind: d.Kind, Range: d.Range, NameRange: d.NameRange},
			start: d.start,
			end:   d.end,
		})
	}

	slices.SortStableFunc(spans, func(a, b span) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	})

	var roots []*Symbol
	var stack []span
	for _, s := range spans {
		for len(stack) > 0 && stack[len(stack)-1].end <= s.start {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			roots = append(roots, s.sym)
		} else {
			parent := stack[len(stack)-1].sym
			parent.Children = append(parent.Children, s.sym)
		}
		stack = append(stack, s)
	}
	return roots
}

// DocumentHighlights returns syntax-tree identifier occurrences matching the
// token at the cursor. It deliberately stays file-local: without type and
// scope information, highlighting same-spelled identifiers in other files
// would imply a certainty Tree-sitter does not provide.
func DocumentHighlights(filename string, src []byte, line, col int) []SymRange {
	entry := detect(filename)
	if entry == nil || len(src) == 0 {
		return nil
	}

	ex := extractors.Get().(*extractor)
	defer extractors.Put(ex)
	file := ex.parse(entry, src, false)
	if file == nil {
		return nil
	}
	defer file.release()
	name, _ := file.identifierAt(line, col)
	if name == "" {
		return nil
	}
	return file.identifiers(name)
}

func (f *parsedFile) identifiers(name string) []SymRange {
	language := f.entry.Language()
	var result []SymRange
	var visit func(*gotreesitter.Node)
	visit = func(node *gotreesitter.Node) {
		if node == nil {
			return
		}
		if node.ChildCount() == 0 {
			if node.EndByte()-node.StartByte() == uint32(len(name)) && isIdentifierNode(language, node, f.src) && node.Text(f.src) == name {
				result = append(result, f.rangeOf(node.StartByte(), node.EndByte()))
			}
			return
		}
		for i := 0; i < node.ChildCount(); i++ {
			visit(node.Child(i))
		}
	}
	visit(f.tree.RootNode())
	return result
}

// FoldingRanges returns conservative declaration-level folds. These are less
// exhaustive than a language server's ranges but stable across all grammars
// with tags support and never invent folds from indentation alone.
func FoldingRanges(filename string, src []byte) []SymRange {
	var result []SymRange
	var add func([]*Symbol)
	add = func(symbols []*Symbol) {
		for _, symbol := range symbols {
			if symbol.Range.EndLine > symbol.Range.StartLine {
				result = append(result, symbol.Range)
			}
			add(symbol.Children)
		}
	}
	add(FileSymbols(filename, src))
	return result
}

type SemanticToken struct {
	Range     SymRange
	Type      string
	Modifiers []string
}

// SemanticTokens classifies a buffer with the grammar's highlight query. It
// is syntactic rather than type-aware, but considerably richer and more
// accurate than lexical colorization when an LSP server is absent.
func SemanticTokens(filename string, src []byte) []SemanticToken {
	entry := detect(filename)
	if entry == nil || entry.HighlightQuery == "" || len(src) == 0 {
		return nil
	}

	ex := extractors.Get().(*extractor)
	defer extractors.Put(ex)
	highlighter := ex.highlighters[entry.Name]
	if highlighter == nil {
		options := []gotreesitter.HighlighterOption{}
		if entry.TokenSourceFactory != nil {
			options = append(options, gotreesitter.WithTokenSourceFactory(func(source []byte) gotreesitter.TokenSource {
				return entry.TokenSourceFactory(source, entry.Language())
			}))
		}
		var err error
		highlighter, err = gotreesitter.NewHighlighter(entry.Language(), entry.HighlightQuery, options...)
		if err != nil {
			return nil
		}
		ex.highlighters[entry.Name] = highlighter
	}

	li := newLineIndex(src)
	declarations := make(map[SymRange]string)
	var collectDeclarations func([]*Symbol)
	collectDeclarations = func(symbols []*Symbol) {
		for _, symbol := range symbols {
			if tokenType := semanticTypeForKind(symbol.Kind); tokenType != "" {
				declarations[symbol.NameRange] = tokenType
			}
			collectDeclarations(symbol.Children)
		}
	}
	collectDeclarations(FileSymbols(filename, src))
	result := make([]SemanticToken, 0)
	for _, highlight := range highlighter.Highlight(src) {
		tokenType, modifiers, ok := semanticCapture(highlight.Capture)
		if !ok {
			continue
		}
		start := highlight.StartByte
		for start < highlight.EndByte {
			line, col := li.lspPos(start)
			end := highlight.EndByte
			if line+1 < len(li.starts) && li.starts[line+1] <= end {
				end = li.starts[line+1]
				for end > start && (src[end-1] == '\n' || src[end-1] == '\r') {
					end--
				}
			}
			endLine, endCol := li.lspPos(end)
			if endLine == line && endCol > col {
				tokenRange := SymRange{StartLine: line, StartCol: col, EndLine: endLine, EndCol: endCol}
				if declarationType := declarations[tokenRange]; declarationType != "" {
					tokenType = declarationType
					modifiers = appendUnique(modifiers, "declaration")
				}
				result = append(result, SemanticToken{
					Range:     tokenRange,
					Type:      tokenType,
					Modifiers: modifiers,
				})
			}
			if line+1 >= len(li.starts) || li.starts[line+1] <= start {
				break
			}
			start = li.starts[line+1]
		}
	}
	return result
}

func semanticTypeForKind(kind Kind) string {
	switch kind {
	case KindFunction, KindConstructor:
		return "function"
	case KindMethod:
		return "method"
	case KindClass:
		return "class"
	case KindInterface:
		return "interface"
	case KindType:
		return "type"
	case KindModule:
		return "namespace"
	case KindConstant, KindVariable:
		return "variable"
	default:
		return ""
	}
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func semanticCapture(capture string) (string, []string, bool) {
	parts := strings.Split(capture, ".")
	var tokenType string
	for _, part := range parts {
		switch part {
		case "comment":
			tokenType = "comment"
		case "string", "character":
			tokenType = "string"
		case "number", "numeric", "float":
			tokenType = "number"
		case "keyword":
			tokenType = "keyword"
		case "operator":
			tokenType = "operator"
		case "method":
			tokenType = "method"
		case "function":
			if tokenType == "" {
				tokenType = "function"
			}
		case "class":
			tokenType = "class"
		case "interface":
			tokenType = "interface"
		case "struct":
			tokenType = "struct"
		case "enum":
			tokenType = "enum"
		case "type":
			if tokenType == "" {
				tokenType = "type"
			}
		case "parameter":
			tokenType = "parameter"
		case "variable":
			if tokenType == "" {
				tokenType = "variable"
			}
		case "property", "field":
			tokenType = "property"
		case "namespace", "module":
			tokenType = "namespace"
		case "macro":
			tokenType = "macro"
		case "label":
			tokenType = "label"
		case "regexp", "regex":
			tokenType = "regexp"
		case "decorator", "attribute":
			tokenType = "decorator"
		}
	}
	if tokenType == "" {
		return "", nil, false
	}
	modifiers := make([]string, 0, 2)
	for _, part := range parts {
		switch part {
		case "declaration", "definition", "readonly", "static", "deprecated", "abstract", "async", "modification", "documentation":
			modifiers = append(modifiers, part)
		case "builtin":
			modifiers = append(modifiers, "defaultLibrary")
		}
	}
	return tokenType, modifiers, true
}
