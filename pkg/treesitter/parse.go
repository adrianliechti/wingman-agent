// Package treesitter provides syntactic code intelligence for files without
// a language server: per-file outlines and highlighting, plus cross-file
// lookups that scan the workspace on demand instead of keeping an index.
package treesitter

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const maxFileBytes = 2 << 20

type Kind string

const (
	KindFunction    Kind = "function"
	KindMethod      Kind = "method"
	KindClass       Kind = "class"
	KindInterface   Kind = "interface"
	KindType        Kind = "type"
	KindConstructor Kind = "constructor"
	KindModule      Kind = "module"
	KindConstant    Kind = "constant"
	KindVariable    Kind = "variable"
)

func kindFromTag(tagKind string) (Kind, bool) {
	switch tagKind {
	case "definition.function":
		return KindFunction, true
	case "definition.method":
		return KindMethod, true
	case "definition.class":
		return KindClass, true
	case "definition.interface":
		return KindInterface, true
	case "definition.type":
		return KindType, true
	case "definition.constructor":
		return KindConstructor, true
	case "definition.module", "definition.namespace":
		return KindModule, true
	case "definition.constant":
		return KindConstant, true
	case "definition.variable":
		return KindVariable, true
	}
	return "", false
}

func isCallTag(tagKind string) bool {
	return tagKind == "reference.call" || tagKind == "reference.send"
}

func isTypeKind(k Kind) bool {
	return k == KindClass || k == KindInterface || k == KindType
}

var importQueries = map[string]string{
	"go": `(import_spec path: (interpreted_string_literal) @path)`,

	"python": `(import_statement (dotted_name) @path)
(import_statement (aliased_import (dotted_name) @path))
(import_from_statement module_name: (dotted_name) @path)
(import_from_statement module_name: (relative_import) @rel)`,

	"javascript": `(import_statement source: (string) @path)
(export_statement source: (string) @path)`,

	"typescript": `(import_statement source: (string) @path)
(export_statement source: (string) @path)`,

	"tsx": `(import_statement source: (string) @path)
(export_statement source: (string) @path)`,
}

var tagsAugment = map[string]string{
	"go":         `(type_spec name: (type_identifier) @name) @definition.type`,
	"javascript": boundFunctionTags,
	"typescript": boundFunctionTags,
	"tsx":        boundFunctionTags,
}

// The grammar's inferred tags omit named arrow/function expressions. These
// are common module entry points (including React components); capturing the
// declarator also gives calls inside the body their correct enclosing owner.
const boundFunctionTags = `(variable_declarator
  name: (identifier) @name
  value: [(arrow_function) (function_expression) (generator_function)]) @definition.function`

var hierarchyQueries = map[string]string{
	"go": `(field_declaration !name (type_identifier) @inherits)
(field_declaration !name (qualified_type name: (type_identifier) @inherits))
(interface_type (type_elem (type_identifier) @inherits))`,

	"python": `(class_definition superclasses: (argument_list (identifier) @inherits))
(class_definition superclasses: (argument_list (attribute attribute: (identifier) @inherits)))`,

	"javascript": `(class_heritage (identifier) @inherits)
(class_heritage (member_expression property: (property_identifier) @inherits))`,

	"typescript": `(extends_clause (identifier) @inherits)
(extends_clause (member_expression property: (property_identifier) @inherits))
(implements_clause (type_identifier) @implements)
(extends_type_clause (type_identifier) @inherits)`,

	"tsx": `(extends_clause (identifier) @inherits)
(extends_clause (member_expression property: (property_identifier) @inherits))
(implements_clause (type_identifier) @implements)
(extends_type_clause (type_identifier) @inherits)`,
}

// extractor holds tree-sitter state that is not safe for concurrent use;
// borrow one from extractors per goroutine.
type extractor struct {
	parsers      map[string]*gotreesitter.Parser
	taggers      map[string]*gotreesitter.Tagger
	queries      map[string]*gotreesitter.Query
	highlighters map[string]*gotreesitter.Highlighter
}

var extractors = sync.Pool{New: func() any {
	return &extractor{
		parsers:      map[string]*gotreesitter.Parser{},
		taggers:      map[string]*gotreesitter.Tagger{},
		queries:      map[string]*gotreesitter.Query{},
		highlighters: map[string]*gotreesitter.Highlighter{},
	}
}}

func (ex *extractor) parser(entry *grammars.LangEntry) *gotreesitter.Parser {
	p := ex.parsers[entry.Name]
	if p == nil {
		p = gotreesitter.NewParser(entry.Language())
		ex.parsers[entry.Name] = p
	}
	return p
}

func (ex *extractor) tagger(entry *grammars.LangEntry) *gotreesitter.Tagger {
	if t, ok := ex.taggers[entry.Name]; ok {
		return t
	}
	var t *gotreesitter.Tagger
	if q := grammars.ResolveTagsQuery(*entry); q != "" {
		if aug := tagsAugment[entry.Name]; aug != "" {
			q += "\n" + aug
		}
		t, _ = gotreesitter.NewTagger(entry.Language(), q)
	}
	ex.taggers[entry.Name] = t
	return t
}

func (ex *extractor) query(kind string, entry *grammars.LangEntry, sources map[string]string) *gotreesitter.Query {
	key := kind + ":" + entry.Name
	if q, ok := ex.queries[key]; ok {
		return q
	}
	var q *gotreesitter.Query
	if source := sources[entry.Name]; source != "" {
		q, _ = gotreesitter.NewQuery(source, entry.Language())
	}
	ex.queries[key] = q
	return q
}

// parsedFile is one parse shared by the extractions a query needs.
type parsedFile struct {
	entry *grammars.LangEntry
	src   []byte
	tree  *gotreesitter.Tree
	lines *lineIndex
	tags  []gotreesitter.Tag
}

func (ex *extractor) parse(entry *grammars.LangEntry, src []byte, withTags bool) *parsedFile {
	tree, err := ex.parser(entry).Parse(src)
	if err != nil || tree.RootNode() == nil {
		if tree != nil {
			tree.Release()
		}
		return nil
	}
	file := &parsedFile{entry: entry, src: src, tree: tree, lines: newLineIndex(src)}
	if withTags {
		if tagger := ex.tagger(entry); tagger != nil {
			file.tags = tagger.TagTree(tree)
		}
	}
	return file
}

func (f *parsedFile) release() { f.tree.Release() }

func (f *parsedFile) rangeOf(start, end uint32) SymRange {
	sl, sc := f.lines.lspPos(start)
	el, ec := f.lines.lspPos(end)
	return SymRange{StartLine: sl, StartCol: sc, EndLine: el, EndCol: ec}
}

// definitions lists tagged declarations, deduplicating the overlap between
// built-in tag queries and our augmentations.
func (f *parsedFile) definitions(rel string) []Definition {
	type key struct {
		start uint32
		name  string
	}
	var out []Definition
	seen := map[key]int{}
	for _, t := range f.tags {
		kind, ok := kindFromTag(t.Kind)
		if !ok {
			continue
		}
		if i, dup := seen[key{t.Range.StartByte, t.Name}]; dup {
			if out[i].Kind == KindType && isTypeKind(kind) {
				out[i].Kind = kind
			}
			continue
		}
		nameStart, nameEnd := t.NameRange.StartByte, t.NameRange.EndByte
		if nameEnd == 0 {
			nameStart, nameEnd = t.Range.StartByte, t.Range.StartByte
		}
		seen[key{t.Range.StartByte, t.Name}] = len(out)
		out = append(out, Definition{
			Name:      t.Name,
			Kind:      kind,
			File:      rel,
			Lang:      f.entry.Name,
			Range:     f.rangeOf(t.Range.StartByte, t.Range.EndByte),
			NameRange: f.rangeOf(nameStart, nameEnd),
			start:     t.Range.StartByte,
			end:       t.Range.EndByte,
		})
	}
	return out
}

// enclosing returns the innermost definition containing the byte offset.
func enclosing(defs []Definition, offset uint32, typesOnly bool) *Definition {
	var best *Definition
	for i := range defs {
		d := &defs[i]
		if offset < d.start || offset >= d.end || typesOnly && !isTypeKind(d.Kind) {
			continue
		}
		if best == nil || d.end-d.start < best.end-best.start {
			best = d
		}
	}
	return best
}

func detect(filename string) *grammars.LangEntry {
	return grammars.DetectLanguage(path.Base(filepath.ToSlash(filename)))
}

// Supported reports whether filename has a grammar with symbol tags.
func Supported(filename string) bool {
	entry := detect(filename)
	if entry == nil {
		return false
	}
	ex := extractors.Get().(*extractor)
	defer extractors.Put(ex)
	return ex.tagger(entry) != nil
}

// identifierAt returns the identifier at (or just before) a position and its
// start offset.
func (f *parsedFile) identifierAt(line, col int) (string, uint32) {
	off, ok := f.lines.byteAt(line, col)
	if !ok {
		return "", 0
	}
	candidates := []uint32{off}
	if off > 0 {
		candidates = append(candidates, off-1)
	}
	for _, o := range candidates {
		n := f.tree.RootNode().NamedNodeAtByte(o)
		if n != nil && isIdentifierNode(f.entry.Language(), n, f.src) {
			return n.Text(f.src), n.StartByte()
		}
	}
	return "", 0
}

func isIdentifierNode(language *gotreesitter.Language, node *gotreesitter.Node, src []byte) bool {
	if node == nil || !node.IsNamed() || node.ChildCount() != 0 || !isIdentifier(node.Text(src)) {
		return false
	}
	typeName := strings.ToLower(node.Type(language))
	return typeName == "identifier" ||
		typeName == "name" ||
		strings.HasSuffix(typeName, "_identifier") ||
		strings.HasSuffix(typeName, "_name")
}

func isIdentifier(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for i, r := range s {
		if unicode.IsLetter(r) || r == '_' || r == '$' || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return true
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// qualifierAt returns the identifier left of the dot in a selector such as
// pkg.Name, or "" for bare names and longer chains.
func qualifierAt(src []byte, nameStart uint32) string {
	i := int(nameStart) - 1
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		i--
	}
	if i < 0 || src[i] != '.' {
		return ""
	}
	i--
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		i--
	}
	end := i
	for i >= 0 && isIdentByte(src[i]) {
		i--
	}
	if end == i || i >= 0 && src[i] == '.' {
		return ""
	}
	return string(src[i+1 : end+1])
}

func isSelectorCall(src []byte, nameStart uint32) bool {
	for i := int(nameStart) - 1; i >= 0; i-- {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case '.':
			return true
		default:
			return false
		}
	}
	return false
}

func langFamily(lang string) string {
	switch lang {
	case "javascript", "jsx", "typescript", "tsx":
		return "js"
	}
	return lang
}

// goUnexported reports whether a Go identifier is package-private, so name
// matches must stay within its directory.
func goUnexported(lang, name string) bool {
	if lang != "go" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	return !unicode.IsUpper(r)
}

// Builtin names are derived from the grammar's highlight query captures
// (@function.builtin / @constructor.builtin), not hand-maintained lists.
var builtinsByLang sync.Map

var builtinPredicate = regexp.MustCompile(
	`#(any-of|eq|match)\?\s+@(?:function|constructor)\.builtin[\w.]*((?:\s+"(?:[^"\\]|\\.)*")+)`,
)

var quotedLiteral = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

func builtinNames(entry *grammars.LangEntry) map[string]bool {
	if cached, ok := builtinsByLang.Load(entry.Name); ok {
		return cached.(map[string]bool)
	}

	names := map[string]bool{}
	for _, m := range builtinPredicate.FindAllStringSubmatch(entry.HighlightQuery, -1) {
		for _, q := range quotedLiteral.FindAllStringSubmatch(m[2], -1) {
			switch m[1] {
			case "eq", "any-of":
				if isIdentifier(q[1]) {
					names[q[1]] = true
				}
			case "match":
				for _, alt := range alternation(q[1]) {
					names[alt] = true
				}
			}
		}
	}

	builtinsByLang.Store(entry.Name, names)
	return names
}

func alternation(pattern string) []string {
	inner, ok := strings.CutPrefix(pattern, "^(")
	if !ok {
		return nil
	}
	inner, ok = strings.CutSuffix(inner, ")$")
	if !ok {
		return nil
	}
	var out []string
	for alt := range strings.SplitSeq(inner, "|") {
		if isIdentifier(alt) {
			out = append(out, alt)
		}
	}
	return out
}

func looksMinified(src []byte) bool {
	const maxLine = 2000
	last := -1
	for i := range src {
		if src[i] == '\n' {
			if i-last-1 > maxLine {
				return true
			}
			last = i
		}
	}
	return len(src)-last-1 > maxLine
}

type lineIndex struct {
	src    []byte
	starts []uint32
}

func newLineIndex(src []byte) *lineIndex {
	starts := []uint32{0}
	for i := range src {
		if src[i] == '\n' {
			starts = append(starts, uint32(i+1))
		}
	}
	return &lineIndex{src: src, starts: starts}
}

func (l *lineIndex) line(byteOff uint32) int {
	lo, hi := 0, len(l.starts)
	for lo < hi {
		mid := (lo + hi) / 2
		if l.starts[mid] <= byteOff {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// lspPos converts a byte offset to a zero-based line and UTF-16 column.
func (l *lineIndex) lspPos(byteOff uint32) (line, column int) {
	ln := l.line(byteOff)
	return ln - 1, utf16Len(l.src[l.starts[ln-1]:byteOff])
}

func (l *lineIndex) byteAt(line, col int) (uint32, bool) {
	if line < 0 || line >= len(l.starts) || col < 0 {
		return 0, false
	}
	end := uint32(len(l.src))
	if line+1 < len(l.starts) {
		end = l.starts[line+1]
	}
	off := l.starts[line]
	units := 0
	for units < col && off < end {
		r, size := utf8.DecodeRune(l.src[off:end])
		if r == '\n' {
			break
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
		off += uint32(size)
	}
	return off, true
}

func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
		b = b[size:]
	}
	return n
}
