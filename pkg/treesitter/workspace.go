package treesitter

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	maxDefinitions = 50
	maxReferences  = 1000
	maxCallees     = 256
	hoverLines     = 12
)

var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"out":          true,
	"bin":          true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".tox":         true,
	"testdata":     true,
}

// Location is an LSP-style position: zero-based line and UTF-16 column in a
// slash-separated workspace-relative file.
type Location struct {
	File string
	Line int
	Col  int
}

type Definition struct {
	Name      string
	Kind      Kind
	File      string
	Lang      string
	Range     SymRange
	NameRange SymRange

	start, end uint32
}

// Call is a caller (incoming) or callee (outgoing) with the call-name ranges
// in the calling file. File is empty for callees without a workspace match.
type Call struct {
	Definition
	Sites []SymRange
}

type HoverInfo struct {
	Definition Definition
	Code       string
	Truncated  bool
	Others     int
}

// Workspace answers cross-file queries by scanning the files that mention a
// name. Matches are syntactic and name-based: same-named symbols in the same
// language family are indistinguishable without type information.
type Workspace struct {
	root string
}

func NewWorkspace(root string) *Workspace {
	return &Workspace{root: root}
}

// Definitions finds declarations of the identifier at a position, nearest
// first. src overrides the file's disk content when non-nil.
func (w *Workspace) Definitions(ctx context.Context, file string, src []byte, line, col int) ([]Definition, error) {
	target, err := w.identifier(file, src, line, col)
	if err != nil || target == nil {
		return nil, err
	}
	defs, err := w.definitionsOf(ctx, target)
	if len(defs) > maxDefinitions {
		defs = defs[:maxDefinitions]
	}
	return defs, err
}

// References finds same-named identifier nodes, which excludes comments and
// strings but not unrelated symbols that share the name.
func (w *Workspace) References(ctx context.Context, file string, src []byte, line, col int, includeDeclaration bool) ([]Location, bool, error) {
	target, err := w.identifier(file, src, line, col)
	if err != nil || target == nil {
		return nil, false, err
	}
	locations, err := scan(ctx, w, target.options(!includeDeclaration), func(rel string, f *parsedFile, _ *extractor) []Location {
		declared := map[SymRange]bool{}
		if !includeDeclaration {
			for _, d := range f.definitions(rel) {
				declared[d.NameRange] = true
			}
		}
		var out []Location
		for _, r := range f.identifiers(target.name) {
			if !declared[r] {
				out = append(out, Location{File: rel, Line: r.StartLine, Col: r.StartCol})
			}
		}
		return out
	})
	if err != nil {
		return nil, false, err
	}
	sortLocations(locations)
	truncated := len(locations) > maxReferences
	if truncated {
		locations = locations[:maxReferences]
	}
	return locations, truncated, nil
}

// Implementations returns types whose extends/implements clauses or
// embeddings name the type at a position. Structural typing (Go interfaces,
// TypeScript shapes) is invisible to this.
func (w *Workspace) Implementations(ctx context.Context, file string, src []byte, line, col int) ([]Definition, error) {
	target, err := w.identifier(file, src, line, col)
	if err != nil || target == nil {
		return nil, err
	}
	defs, err := scan(ctx, w, target.options(true), func(rel string, f *parsedFile, ex *extractor) []Definition {
		query := ex.query("hierarchy", f.entry, hierarchyQueries)
		if query == nil {
			return nil
		}
		defs := f.definitions(rel)
		var out []Definition
		for _, match := range query.ExecuteNode(f.tree.RootNode(), f.entry.Language(), f.src) {
			for _, capture := range match.Captures {
				if capture.Node.Text(f.src) != target.name {
					continue
				}
				if owner := enclosing(defs, capture.Node.StartByte(), true); owner != nil && !slices.ContainsFunc(out, func(d Definition) bool { return d.start == owner.start }) {
					out = append(out, *owner)
				}
			}
		}
		return out
	})
	if err != nil {
		return nil, err
	}
	sortDefinitions(defs)
	return defs, nil
}

// Hover returns the nearest declaration of the identifier at a position with
// the start of its source, or nil when nothing matches.
func (w *Workspace) Hover(ctx context.Context, file string, src []byte, line, col int) (*HoverInfo, error) {
	defs, err := w.Definitions(ctx, file, src, line, col)
	if err != nil || len(defs) == 0 {
		return nil, err
	}
	def := defs[0]
	source := src
	if def.File != file || source == nil {
		if source, err = w.readSource(def.File); err != nil {
			return nil, nil
		}
	}
	lines := strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
	start, end := def.Range.StartLine, min(def.Range.EndLine, len(lines)-1)
	if start >= len(lines) {
		return nil, nil
	}
	truncated := end-start+1 > hoverLines
	if truncated {
		end = start + hoverLines - 1
	}
	return &HoverInfo{Definition: def, Code: strings.Join(lines[start:end+1], "\n"), Truncated: truncated, Others: len(defs) - 1}, nil
}

// Symbols lists declarations whose name contains query, case-insensitively,
// exact and prefix matches first, in files include accepts (nil for all).
// A positive limit caps the result; the bool reports that it was reached.
func (w *Workspace) Symbols(ctx context.Context, query string, limit int, include func(file string) bool) ([]Definition, bool, error) {
	lower := strings.ToLower(query)
	opts := scanOptions{tags: true, include: include}
	if lower != "" {
		needle := []byte(lower)
		opts.match = func(src []byte) bool { return bytes.Contains(bytes.ToLower(src), needle) }
	} else if limit > 0 {
		// Every declaration matches, so stop early; a query must see all
		// matches to rank exact ones first.
		opts.limit = limit + 1
	}
	defs, err := scan(ctx, w, opts, func(rel string, f *parsedFile, _ *extractor) []Definition {
		var out []Definition
		for _, d := range f.definitions(rel) {
			if strings.Contains(strings.ToLower(d.Name), lower) {
				out = append(out, d)
			}
		}
		return out
	})
	if err != nil {
		return nil, false, err
	}
	score := func(d Definition) int {
		name := strings.ToLower(d.Name)
		switch {
		case lower == "" || name == lower:
			return 2
		case strings.HasPrefix(name, lower):
			return 1
		}
		return 0
	}
	sortDefinitions(defs)
	sort.SliceStable(defs, func(i, j int) bool { return score(defs[i]) > score(defs[j]) })
	truncated := limit > 0 && len(defs) > limit
	if truncated {
		defs = defs[:limit]
	}
	return defs, truncated, nil
}

// IncomingCalls finds call sites of the name at a position, grouped by the
// innermost enclosing declaration (or the file, for top-level calls).
func (w *Workspace) IncomingCalls(ctx context.Context, file string, src []byte, line, col int) ([]Call, error) {
	target, err := w.identifier(file, src, line, col)
	if err != nil || target == nil {
		return nil, err
	}
	calls, err := scan(ctx, w, target.options(true), func(rel string, f *parsedFile, _ *extractor) []Call {
		defs := f.definitions(rel)
		builtins := builtinNames(f.entry)
		var out []Call
		index := map[*Definition]int{}
		for _, t := range f.tags {
			if !isCallTag(t.Kind) || t.Name != target.name {
				continue
			}
			start, end := t.NameRange.StartByte, t.NameRange.EndByte
			if end == 0 {
				start, end = t.Range.StartByte, t.Range.StartByte+uint32(len(t.Name))
			}
			if builtins[t.Name] && !isSelectorCall(f.src, start) {
				continue
			}
			owner := enclosing(defs, t.Range.StartByte, false)
			i, ok := index[owner]
			if !ok {
				caller := Definition{Name: path.Base(rel), Kind: KindModule, File: rel, Lang: f.entry.Name}
				if owner != nil {
					caller = *owner
				}
				i = len(out)
				index[owner] = i
				out = append(out, Call{Definition: caller})
			}
			out[i].Sites = append(out[i].Sites, f.rangeOf(start, end))
		}
		return out
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(calls, func(a, b Call) int { return compareDefinitions(a.Definition, b.Definition) })
	return calls, nil
}

// OutgoingCalls lists calls made inside the declaration at a position, each
// resolved to the nearest same-named workspace declaration.
func (w *Workspace) OutgoingCalls(ctx context.Context, file string, src []byte, line, col int) ([]Call, error) {
	target, err := w.identifier(file, src, line, col)
	if err != nil || target == nil {
		return nil, err
	}
	caller, err := w.declarationAt(ctx, target)
	if err != nil || caller == nil {
		return nil, err
	}
	callerSrc := src
	if caller.File != file || callerSrc == nil {
		if callerSrc, err = w.readSource(caller.File); err != nil {
			return nil, err
		}
	}

	entry := detect(caller.File)
	ex := extractors.Get().(*extractor)
	f := ex.parse(entry, callerSrc, true)
	if f == nil {
		extractors.Put(ex)
		return nil, nil
	}
	var calls []Call
	var qualifiers []string
	index := map[string]int{}
	builtins := builtinNames(f.entry)
	declarations := f.definitions(caller.File)
	for _, t := range f.tags {
		if !isCallTag(t.Kind) || t.Range.StartByte < caller.start || t.Range.StartByte >= caller.end {
			continue
		}
		if owner := enclosing(declarations, t.Range.StartByte, false); owner == nil || owner.start != caller.start {
			continue
		}
		start, end := t.NameRange.StartByte, t.NameRange.EndByte
		if end == 0 {
			start, end = t.Range.StartByte, t.Range.StartByte+uint32(len(t.Name))
		}
		if builtins[t.Name] && !isSelectorCall(f.src, start) {
			continue
		}
		i, ok := index[t.Name]
		if !ok {
			if len(calls) >= maxCallees {
				continue
			}
			i = len(calls)
			index[t.Name] = i
			calls = append(calls, Call{Definition: Definition{Name: t.Name}})
			qualifiers = append(qualifiers, qualifierAt(f.src, start))
		}
		calls[i].Sites = append(calls[i].Sites, f.rangeOf(start, end))
	}
	imports := ex.imports(f, caller.File)
	f.release()
	extractors.Put(ex)
	if len(calls) == 0 {
		return nil, nil
	}

	opts := scanOptions{family: langFamily(entry.Name), tags: true, override: target.file, source: target.src}
	opts.match = func(src []byte) bool { return containsAnyWord(src, index) }
	defs, err := scan(ctx, w, opts, func(rel string, f *parsedFile, _ *extractor) []Definition {
		var out []Definition
		for _, d := range f.definitions(rel) {
			if _, ok := index[d.Name]; ok {
				out = append(out, d)
			}
		}
		return out
	})
	if err != nil {
		return nil, err
	}
	byName := map[string][]Definition{}
	for _, d := range defs {
		byName[d.Name] = append(byName[d.Name], d)
	}
	for i := range calls {
		near := nearness{file: caller.File, imports: imports, qualifier: qualifiers[i]}
		if candidates := near.rank(byName[calls[i].Name]); len(candidates) > 0 {
			calls[i].Definition = candidates[0]
		}
	}
	slices.SortStableFunc(calls, func(a, b Call) int {
		if (a.File == "") != (b.File == "") {
			if a.File == "" {
				return 1
			}
			return -1
		}
		return compareDefinitions(a.Definition, b.Definition)
	})
	return calls, nil
}

type identifierTarget struct {
	nearness
	src   []byte
	name  string
	line  int
	col   int
	entry *grammars.LangEntry
	local []Definition // declarations in the target's own file
}

func (w *Workspace) identifier(file string, src []byte, line, col int) (*identifierTarget, error) {
	entry := detect(file)
	if entry == nil {
		return nil, nil
	}
	if src == nil {
		data, err := w.readSource(file)
		if err != nil {
			return nil, err
		}
		src = data
	}
	ex := extractors.Get().(*extractor)
	defer extractors.Put(ex)
	f := ex.parse(entry, src, true)
	if f == nil {
		return nil, nil
	}
	defer f.release()
	name, start := f.identifierAt(line, col)
	if name == "" {
		return nil, nil
	}
	return &identifierTarget{
		nearness: nearness{file: file, imports: ex.imports(f, file), qualifier: qualifierAt(src, start)},
		src:      src,
		name:     name,
		line:     line,
		col:      col,
		entry:    entry,
		local:    f.definitions(file),
	}, nil
}

// options scans files of the target's language family that mention its name;
// Go's unexported names are only visible within their package directory.
func (t *identifierTarget) options(tags bool) scanOptions {
	needle := []byte(t.name)
	opts := scanOptions{
		family:   langFamily(t.entry.Name),
		tags:     tags,
		override: t.file,
		source:   t.src,
		match:    func(src []byte) bool { return containsWord(src, needle) },
	}
	if goUnexported(t.entry.Name, t.name) {
		dir := path.Dir(t.file)
		opts.include = func(rel string) bool { return path.Dir(rel) == dir }
	}
	return opts
}

func (w *Workspace) definitionsOf(ctx context.Context, target *identifierTarget) ([]Definition, error) {
	// Unexported Go names are searched everywhere and then preferred within
	// the package, so a misplaced declaration is still found.
	opts := target.options(true)
	opts.include = nil
	defs, err := scan(ctx, w, opts, func(rel string, f *parsedFile, _ *extractor) []Definition {
		var out []Definition
		for _, d := range f.definitions(rel) {
			if d.Name == target.name {
				out = append(out, d)
			}
		}
		return out
	})
	if err != nil {
		return nil, err
	}
	if goUnexported(target.entry.Name, target.name) {
		dir := path.Dir(target.file)
		if local := slices.DeleteFunc(slices.Clone(defs), func(d Definition) bool { return path.Dir(d.File) != dir }); len(local) > 0 {
			defs = local
		}
	}
	return target.rank(defs), nil
}

// declarationAt resolves the declaration a call hierarchy starts from: the
// one named at the position, else the unique nearest same-named one.
func (w *Workspace) declarationAt(ctx context.Context, target *identifierTarget) (*Definition, error) {
	for _, d := range target.local {
		r := d.NameRange
		if d.Name == target.name && r.StartLine == target.line && r.StartCol <= target.col && target.col <= r.EndCol {
			return &d, nil
		}
	}

	defs, err := w.definitionsOf(ctx, target)
	if err != nil || len(defs) == 0 {
		return nil, err
	}
	if len(defs) > 1 && target.proximity(defs[0]) == target.proximity(defs[1]) {
		var choices []string
		for _, d := range defs[:min(len(defs), 10)] {
			choices = append(choices, fmt.Sprintf("%s:%d", d.File, d.NameRange.StartLine+1))
		}
		return nil, fmt.Errorf("%q has %d equally near declarations (%s); pass the position of one declaration", target.name, len(defs), strings.Join(choices, ", "))
	}
	return &defs[0], nil
}

type importPath struct {
	path     string
	relative bool
}

func (ex *extractor) imports(f *parsedFile, file string) []importPath {
	query := ex.query("imports", f.entry, importQueries)
	if query == nil {
		return nil
	}
	var out []importPath
	for _, match := range query.ExecuteNode(f.tree.RootNode(), f.entry.Language(), f.src) {
		for _, capture := range match.Captures {
			text := capture.Node.Text(f.src)
			var value importPath
			if f.entry.Name == "python" {
				value.relative = capture.Name == "rel"
				value.path = normalizePython(text, value.relative)
			} else {
				value.path = strings.Trim(text, "\"'`")
				value.relative = strings.HasPrefix(value.path, ".")
			}
			if value.relative {
				value.path = path.Clean(path.Join(path.Dir(file), value.path))
			}
			if value.path != "" {
				out = append(out, value)
			}
		}
	}
	return out
}

func normalizePython(text string, rel bool) string {
	if !rel {
		return strings.ReplaceAll(text, ".", "/")
	}
	level := 0
	for level < len(text) && text[level] == '.' {
		level++
	}
	rest := strings.ReplaceAll(text[level:], ".", "/")
	return strings.Repeat("../", level-1) + rest
}

func importsDir(imports []importPath, dir string) bool {
	for _, im := range imports {
		if im.relative {
			if im.path == dir || path.Dir(im.path) == dir {
				return true
			}
			continue
		}
		// Module paths end with their workspace directory; Python names the
		// module file itself, so its parent directory counts too.
		for _, candidate := range []string{im.path, path.Dir(im.path)} {
			if dir != "." && (candidate == dir || strings.HasSuffix(candidate, "/"+dir)) {
				return true
			}
		}
	}
	return false
}

// nearness ranks same-named declarations relative to the file using them.
type nearness struct {
	file      string
	imports   []importPath
	qualifier string // identifier before the dot at the use site, if any
}

func (n nearness) proximity(d Definition) int {
	dir := path.Dir(d.File)
	imported := importsDir(n.imports, dir)
	switch {
	case imported && n.qualifier != "" && path.Base(dir) == n.qualifier:
		return 4
	case d.File == n.file:
		return 3
	case dir == path.Dir(n.file):
		return 2
	case imported:
		return 1
	}
	return 0
}

// rank orders declarations: an imported package named by the qualifier, the
// same file, the same directory, then other imported directories.
func (n nearness) rank(defs []Definition) []Definition {
	sortDefinitions(defs)
	sort.SliceStable(defs, func(i, j int) bool {
		return n.proximity(defs[i]) > n.proximity(defs[j])
	})
	return defs
}

func compareDefinitions(a, b Definition) int {
	if a.File != b.File {
		return strings.Compare(a.File, b.File)
	}
	if a.NameRange.StartLine != b.NameRange.StartLine {
		return a.NameRange.StartLine - b.NameRange.StartLine
	}
	return a.NameRange.StartCol - b.NameRange.StartCol
}

func sortDefinitions(defs []Definition) {
	slices.SortStableFunc(defs, compareDefinitions)
}

func sortLocations(out []Location) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Col < out[j].Col
	})
}

func containsWord(src, word []byte) bool {
	for off := 0; off < len(src); {
		i := bytes.Index(src[off:], word)
		if i < 0 {
			return false
		}
		i += off
		end := i + len(word)
		if (i == 0 || !isIdentByte(src[i-1])) && (end == len(src) || !isIdentByte(src[end])) {
			return true
		}
		off = i + 1
	}
	return false
}

func containsAnyWord[V any](src []byte, words map[string]V) bool {
	for i := 0; i < len(src); {
		if !isIdentByte(src[i]) {
			i++
			continue
		}
		start := i
		for i < len(src) && isIdentByte(src[i]) {
			i++
		}
		if _, ok := words[string(src[start:i])]; ok {
			return true
		}
	}
	return false
}

type sourceFile struct {
	rel   string
	entry *grammars.LangEntry
}

type scanOptions struct {
	family   string // language family; empty scans every supported language
	include  func(rel string) bool
	match    func(src []byte) bool
	tags     bool
	limit    int    // stop starting new files once this many results exist
	override string // file whose content is source instead of the disk copy
	source   []byte
}

// scan parses matching source files in parallel and concatenates extract's
// results in file order.
func scan[T any](ctx context.Context, w *Workspace, opts scanOptions, extract func(rel string, f *parsedFile, ex *extractor) []T) ([]T, error) {
	files, err := w.sourceFiles(ctx, opts)
	if err != nil {
		return nil, err
	}
	results := make([][]T, len(files))
	var next, found atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(files)) {
		wg.Go(func() {
			ex := extractors.Get().(*extractor)
			defer extractors.Put(ex)
			for ctx.Err() == nil && (opts.limit <= 0 || found.Load() < int64(opts.limit)) {
				i := int(next.Add(1) - 1)
				if i >= len(files) {
					return
				}
				src := opts.source
				if files[i].rel != opts.override || src == nil {
					var readErr error
					if src, readErr = w.readSource(files[i].rel); readErr != nil {
						continue
					}
				}
				if opts.match != nil && !opts.match(src) {
					continue
				}
				f := ex.parse(files[i].entry, src, opts.tags)
				if f == nil {
					continue
				}
				results[i] = extract(files[i].rel, f, ex)
				f.release()
				found.Add(int64(len(results[i])))
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return slices.Concat(results...), nil
}

func (w *Workspace) sourceFiles(ctx context.Context, opts scanOptions) ([]sourceFile, error) {
	var files []sourceFile
	add := func(rel string) {
		entry := detect(rel)
		if entry != nil && (opts.family == "" || langFamily(entry.Name) == opts.family) && (opts.include == nil || opts.include(rel)) {
			files = append(files, sourceFile{rel: rel, entry: entry})
		}
	}
	err := pathutil.WalkDir(w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != w.root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if rel, err := filepath.Rel(w.root, p); err == nil {
			add(filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if opts.override != "" && !slices.ContainsFunc(files, func(f sourceFile) bool { return f.rel == opts.override }) {
		add(opts.override)
	}
	return files, nil
}

func (w *Workspace) readSource(rel string) ([]byte, error) {
	abs := filepath.Join(w.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s is not a regular source file of at most %d bytes", rel, maxFileBytes)
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if looksMinified(src) {
		return nil, fmt.Errorf("%s looks minified", rel)
	}
	return src, nil
}
