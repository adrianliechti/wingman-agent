package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CallResolver interface {
	ResolveCall(ctx context.Context, file string, line, column int) (defFile string, defLine int, ok bool)
}

type ResolvedLocation struct {
	File string
	Line int
}

// ImplementationResolver is an optional CallResolver capability for edges to
// concrete implementations of interface call sites.
type ImplementationResolver interface {
	ResolveImplementations(ctx context.Context, file string, line, column int) []ResolvedLocation
}

type Engine struct {
	root      string
	cachePath string
	resolver  CallResolver

	buildMu    sync.Mutex
	refreshing atomic.Bool

	staleMu  sync.Mutex
	staleAt  time.Time
	staleVal bool

	mu           sync.RWMutex
	graph        *Graph              // immutable after publication; mu guards replacement
	files        map[string]fileMeta // immutable after publication
	indexedFiles int
	skipped      []CoverageIssue
	indexedAt    time.Time
}

var staleCheckTTL = 10 * time.Second

type Option func(*Engine)

func WithResolver(r CallResolver) Option {
	return func(e *Engine) { e.resolver = r }
}

func New(root, cachePath string, opts ...Option) *Engine {
	e := &Engine{root: root, cachePath: cachePath}
	for _, o := range opts {
		o(e)
	}
	return e
}

type Status struct {
	Indexed   bool
	IndexedAt time.Time
	Files     int
	Nodes     int
	Edges     int
	Skipped   []CoverageIssue
}

func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.graph == nil {
		return Status{}
	}
	return Status{
		Indexed:   true,
		IndexedAt: e.indexedAt,
		Files:     e.indexedFiles,
		Nodes:     len(e.graph.Nodes),
		Edges:     len(e.graph.Edges),
		Skipped:   append([]CoverageIssue(nil), e.skipped...),
	}
}

func (e *Engine) EdgeStats() map[Provenance]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[Provenance]int{}
	if e.graph == nil {
		return out
	}
	for _, ed := range e.graph.Edges {
		via := ed.Via
		if via == "" {
			via = ViaName
		}
		out[via]++
	}
	return out
}

func (e *Engine) StatusOrLoad() Status {
	e.tryLoadCache()
	return e.Status()
}

func (e *Engine) tryLoadCache() *Graph {
	e.mu.RLock()
	g := e.graph
	e.mu.RUnlock()
	if g != nil || e.cachePath == "" {
		return g
	}

	loaded, err := loadSnapshot(e.cachePath)
	if err != nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.graph == nil {
		e.graph = loaded.graph
		e.files = loaded.files
		e.indexedFiles = loaded.indexedFiles
		e.skipped = loaded.skipped
		e.indexedAt = loaded.indexedAt
	}
	return e.graph
}

func (e *Engine) DeadCode(ctx context.Context, limit int) ([]*Node, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return nil, err
	}
	return g.deadCode(limit), nil
}

func (e *Engine) DetectChanges(ctx context.Context, since string) (Changes, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return Changes{}, err
	}

	changed, err := gitChanges(e.root, since)
	if err != nil {
		return Changes{}, err
	}

	return affectedNodes(g, changed), nil
}

func (e *Engine) Index(ctx context.Context) (Status, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	return e.indexLocked(ctx)
}

func (e *Engine) indexLocked(ctx context.Context) (Status, error) {
	g, files, stats, err := indexRepo(ctx, e.root, e.resolver)
	if err != nil {
		return Status{}, err
	}

	now := time.Now()

	e.mu.Lock()
	e.graph = g
	e.files = files
	e.indexedFiles = stats.Files
	e.skipped = stats.Skipped
	e.indexedAt = now
	e.mu.Unlock()

	if e.cachePath != "" {
		_ = saveSnapshot(e.cachePath, g, files, stats.Files, stats.Skipped, now)
	}

	e.staleMu.Lock()
	e.staleAt = now
	e.staleVal = false
	e.staleMu.Unlock()

	return e.Status(), nil
}

// ensureIndexed never blocks a query on freshness: a missing graph builds
// synchronously, a stale one is served while one background refresh runs.
func (e *Engine) ensureIndexed(ctx context.Context) (*Graph, error) {
	if g := e.tryLoadCache(); g != nil {
		if e.IsStale(ctx) {
			e.refreshInBackground()
		}
		return g, nil
	}

	e.buildMu.Lock()
	defer e.buildMu.Unlock()

	e.mu.RLock()
	g := e.graph
	e.mu.RUnlock()
	if g != nil {
		return g, nil
	}

	if _, err := e.indexLocked(ctx); err != nil {
		return nil, err
	}

	e.mu.RLock()
	g = e.graph
	e.mu.RUnlock()
	return g, nil
}

func (e *Engine) refreshInBackground() {
	if !e.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, _ = e.Index(ctx)
	}()
}

// IsStale reports whether any file's size/mtime or the file set differs from
// the workspace, re-walking at most once per staleCheckTTL. False with no
// graph means "not indexed", not "fresh".
func (e *Engine) IsStale(ctx context.Context) bool {
	if e.tryLoadCache() == nil {
		return false
	}

	e.staleMu.Lock()
	if time.Since(e.staleAt) < staleCheckTTL {
		stale := e.staleVal
		e.staleMu.Unlock()
		return stale
	}
	e.staleMu.Unlock()

	stale := e.checkStale(ctx)
	e.staleMu.Lock()
	e.staleAt = time.Now()
	e.staleVal = stale
	e.staleMu.Unlock()
	return stale
}

func (e *Engine) checkStale(ctx context.Context) bool {
	e.mu.RLock()
	files := e.files
	e.mu.RUnlock()

	paths, err := collectFiles(ctx, e.root)
	if err != nil || len(paths) != len(files) {
		return true
	}
	for _, abs := range paths {
		rel, err := filepath.Rel(e.root, abs)
		if err != nil {
			return true
		}
		rel = filepath.ToSlash(rel)
		want, ok := files[rel]
		if !ok {
			return true
		}
		info, err := os.Stat(abs)
		if err != nil || want.Size != info.Size() || want.MTime != info.ModTime().UnixNano() {
			return true
		}
	}
	return false
}

func (e *Engine) Search(ctx context.Context, opts SearchOpts) ([]*Node, error) {
	res, err := e.SearchPage(ctx, opts)
	return res.Nodes, err
}

func (e *Engine) SearchPage(ctx context.Context, opts SearchOpts) (SearchResult, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	return g.searchPage(opts), nil
}

func (e *Engine) Architecture(ctx context.Context) (Arch, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return Arch{}, err
	}
	return g.architecture(), nil
}

func (e *Engine) Deps(ctx context.Context, target string, depth int) (DepsResult, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return DepsResult{}, err
	}
	return g.deps(g.resolveModule(filepath.ToSlash(target)), depth), nil
}

func (e *Engine) Hierarchy(ctx context.Context, name, file string) (HierarchyResult, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return HierarchyResult{}, err
	}

	cands := g.resolve(name, file)
	if len(cands) == 0 {
		return HierarchyResult{}, g.notFoundErr(name, file)
	}

	node := cands[0]
	for _, c := range cands {
		if isTypeKind(c.Kind) {
			node = c
			break
		}
	}
	res := g.hierarchy(node.ID)
	res.Others = othersOf(cands, node)
	return res, nil
}

func (g *Graph) notFoundErr(name, file string) error {
	message := fmt.Sprintf("no symbol named %q in the graph", name)
	if file != "" {
		message = fmt.Sprintf("no symbol named %q in file matching %q", name, file)
	}
	// Models often supply Type.Method. The index accepts path qualifiers;
	// show actual definitions instead of discarding a potentially meaningful
	// receiver/class qualifier and silently selecting the wrong method.
	if i := strings.LastIndexAny(name, ".:/"); i > 0 && i+1 < len(name) {
		candidates := g.resolve(name[i+1:], file)
		if len(candidates) > 0 {
			message += fmt.Sprintf(". Use a bare name or path:Name. Same-name definitions (verify receiver/class; showing %d of %d):", min(len(candidates), 5), len(candidates))
			for _, n := range candidates[:min(len(candidates), 5)] {
				message += fmt.Sprintf("\n- %s:%s (line %d)", n.File, n.Name, n.StartLine)
			}
		}
	}
	return errors.New(message)
}

func othersOf(cands []*Node, chosen *Node) []*Node {
	var out []*Node
	for _, c := range cands {
		if c != chosen {
			out = append(out, c)
		}
	}
	return out
}

func isTypeKind(k Kind) bool {
	return k == KindClass || k == KindInterface || k == KindType
}

func (e *Engine) Tests(ctx context.Context, name, file string) (TestsResult, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return TestsResult{}, err
	}

	cands := g.resolve(name, file)
	if len(cands) == 0 {
		return TestsResult{}, g.notFoundErr(name, file)
	}
	res := g.testsFor(cands[0])
	res.Others = othersOf(cands, cands[0])
	return res, nil
}

func (e *Engine) Similar(ctx context.Context, name, file string, limit int) (SimilarResult, error) {
	if limit <= 0 {
		limit = 15
	}
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return SimilarResult{}, err
	}

	cands := g.resolve(name, file)
	if len(cands) == 0 {
		return SimilarResult{}, g.notFoundErr(name, file)
	}
	node := cands[0]
	for _, c := range cands {
		if isCallable(c.Kind) {
			node = c
			break
		}
	}
	if !isCallable(node.Kind) {
		return SimilarResult{}, fmt.Errorf("find_similar works on functions/methods/constructors, but %q is a %s", node.Name, node.Kind)
	}
	return SimilarResult{Target: node, Matches: g.similar(node, limit), Others: othersOf(cands, node)}, nil
}

func (e *Engine) CoChanges(ctx context.Context, file string, limit int) (CoChangesResult, error) {
	if limit <= 0 {
		limit = 15
	}
	return coChanges(e.root, filepath.ToSlash(file), limit)
}

type Snippet struct {
	Node   *Node
	Code   string
	Others []*Node
}

func (e *Engine) Snippet(ctx context.Context, name, file string) (Snippet, error) {
	g, err := e.ensureIndexed(ctx)
	if err != nil {
		return Snippet{}, err
	}

	cands := g.resolve(name, file)

	if len(cands) == 0 {
		return Snippet{}, g.notFoundErr(name, file)
	}

	node := cands[0]
	code, err := e.readLines(node.File, node.StartLine, node.EndLine)
	if err != nil {
		return Snippet{}, err
	}
	return Snippet{Node: node, Code: code, Others: othersOf(cands, node)}, nil
}

func (e *Engine) readLines(rel string, start, end int) (string, error) {
	if start <= 0 {
		start = 1
	}
	clean := filepath.FromSlash(rel)
	full := filepath.Join(e.root, clean)

	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	if start > len(lines) {
		return "", errors.New("line range out of bounds")
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
	}
	return b.String(), nil
}
