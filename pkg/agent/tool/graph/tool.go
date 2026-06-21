package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/graph"
)

func NewTools(engine *graph.Engine) []tool.Tool {
	if engine == nil {
		return nil
	}
	return []tool.Tool{graphTool(engine)}
}

func graphTool(engine *graph.Engine) tool.Tool {
	return tool.Tool{
		Name: "code_graph",
		Description: strings.Join([]string{
			"Query a tree-sitter knowledge graph of the codebase: functions, methods, classes, and their call relationships across many languages.",
			"Prefer this over repeated grep/read cycles to understand structure, find symbols, or follow call chains.",
			"Works with no language server installed. When one is available, prefer the `lsp` tool for precise go-to-definition/references, type info (hover), and diagnostics; use `code_graph` for whole-repo structure, cross-language search, and multi-hop call/dependency traversal.",
			"Operations:",
			"- `index`: build or rebuild the graph. Run once per session, or again after large changes.",
			"- `status`: report whether the graph is indexed and its size.",
			"- `search`: find definitions by name (case-insensitive regex). Optional `kind` and `file` filters.",
			"- `trace`: follow call paths from `symbol`. `direction=callees` (what it calls) or `callers` (what calls it). Optional `target` to find a path to a specific symbol. Hops marked `⇢` are ambiguous name-based guesses; `→` are precise.",
			"- `architecture`: high-level overview — languages, layers, modules, entry points, and call hotspots.",
			"- `dead_code`: functions/methods with no detected callers (excluding entry points). Treat as candidates — callers via reflection, exported API, or another language (e.g. JS↔Go bindings) are not visible.",
			"- `changes`: uncommitted git changes mapped to the affected definitions.",
			"- `deps`: module dependency graph for a `file`/dir — what it imports (depends on), what imports it (depended by), external packages, and with `depth`>1 transitive deps.",
			"- `hierarchy`: type relationships for `symbol` — what it extends/implements, plus its subtypes/implementers. Structural (extends/implements/bases/embedding); Go's implicit interface satisfaction is not captured — use `lsp` goToImplementation for that.",
			"- `snippet`: return the source of a definition by `symbol` (optionally disambiguated by `file`).",
			"- `tests`: for a production `symbol`, the test functions that exercise it; for a test `symbol`, the production symbols it covers. Derived from call edges into/out of test files — direct calls only.",
			"- `co_changes`: files that historically change together with `file`, from git history (commit co-occurrence). Surfaces coupling the call graph misses (config, docs, sibling implementations).",
			"The graph auto-builds on first use if not yet indexed.",
		}, "\n"),
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type":        "string",
					"enum":        []string{"index", "status", "search", "trace", "architecture", "dead_code", "changes", "deps", "hierarchy", "snippet", "tests", "co_changes"},
					"description": "The graph operation to perform.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "search: case-insensitive regex matched against symbol names.",
				},
				"symbol": map[string]any{
					"type":        "string",
					"description": "trace/snippet/hierarchy/tests: the symbol name to start from or fetch.",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "trace: optional destination symbol name; returns call paths reaching it.",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"callees", "callers"},
					"description": "trace: callees (outgoing, default) or callers (incoming).",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"function", "method", "class", "interface", "type", "constructor", "module", "constant", "variable"},
					"description": "search: restrict to a definition kind.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "search/snippet/tests: restrict to files whose path contains this substring. deps: the module/dir or file path to query. co_changes: the file path to analyze.",
				},
				"depth": map[string]any{
					"type":        "integer",
					"description": "trace: maximum path depth (default 8). deps: transitive depth — >1 includes indirect dependencies.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "search: maximum results (default 50).",
				},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			operation, _ := args["operation"].(string)

			switch operation {
			case "index":
				status, err := engine.Index(ctx)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Indexed %d files: %d definitions, %d call edges%s.",
					status.Files, status.Nodes, status.Edges, edgeBreakdown(engine.EdgeStats())), nil

			case "status":
				st := engine.StatusOrLoad()
				if !st.Indexed {
					return "Not indexed yet. Run operation \"index\" (or any query auto-builds it).", nil
				}
				return fmt.Sprintf("Indexed at %s: %d files, %d definitions, %d call edges%s.",
					st.IndexedAt.Format(time.RFC3339), st.Files, st.Nodes, st.Edges, edgeBreakdown(engine.EdgeStats())), nil

			case "search":
				query, _ := args["query"].(string)
				limit, _ := tool.IntArg(args, "limit")
				opts := graph.SearchOpts{
					Query: query,
					Kind:  graph.Kind(strings.TrimSpace(stringArg(args, "kind"))),
					File:  stringArg(args, "file"),
					Limit: limit,
				}
				nodes, err := engine.Search(ctx, opts)
				if err != nil {
					return "", err
				}
				return formatNodes(nodes), nil

			case "trace":
				symbol := strings.TrimSpace(stringArg(args, "symbol"))
				if symbol == "" {
					return "", fmt.Errorf("symbol is required for trace")
				}
				callers := stringArg(args, "direction") == "callers"
				depth, _ := tool.IntArg(args, "depth")
				res, err := engine.Trace(ctx, symbol, strings.TrimSpace(stringArg(args, "target")), callers, depth)
				if err != nil {
					return "", err
				}
				return formatTrace(res, callers), nil

			case "architecture":
				arch, err := engine.Architecture(ctx)
				if err != nil {
					return "", err
				}
				return formatArch(arch), nil

			case "dead_code":
				limit, _ := tool.IntArg(args, "limit")
				nodes, err := engine.DeadCode(ctx, limit)
				if err != nil {
					return "", err
				}
				if len(nodes) == 0 {
					return "No dead code found (every callable has a detected caller).", nil
				}
				return formatNodes(nodes), nil

			case "changes":
				changes, err := engine.DetectChanges(ctx)
				if err != nil {
					return "", err
				}
				return formatChanges(changes), nil

			case "deps":
				target := strings.TrimSpace(stringArg(args, "file"))
				if target == "" {
					target = strings.TrimSpace(stringArg(args, "symbol"))
				}
				if target == "" {
					return "", fmt.Errorf("file (a module/dir or file path) is required for deps")
				}
				depth, _ := tool.IntArg(args, "depth")
				res, err := engine.Deps(ctx, target, depth)
				if err != nil {
					return "", err
				}
				return formatDeps(res), nil

			case "hierarchy":
				symbol := strings.TrimSpace(stringArg(args, "symbol"))
				if symbol == "" {
					return "", fmt.Errorf("symbol is required for hierarchy")
				}
				res, err := engine.Hierarchy(ctx, symbol, stringArg(args, "file"))
				if err != nil {
					return "", err
				}
				return formatHierarchy(res), nil

			case "snippet":
				symbol := strings.TrimSpace(stringArg(args, "symbol"))
				if symbol == "" {
					return "", fmt.Errorf("symbol is required for snippet")
				}
				snip, err := engine.Snippet(ctx, symbol, stringArg(args, "file"))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%s\n%s", nodeLabel(snip.Node), snip.Code), nil

			case "tests":
				symbol := strings.TrimSpace(stringArg(args, "symbol"))
				if symbol == "" {
					return "", fmt.Errorf("symbol is required for tests")
				}
				res, err := engine.Tests(ctx, symbol, stringArg(args, "file"))
				if err != nil {
					return "", err
				}
				return formatTests(res), nil

			case "co_changes":
				file := strings.TrimSpace(stringArg(args, "file"))
				if file == "" {
					return "", fmt.Errorf("file is required for co_changes")
				}
				limit, _ := tool.IntArg(args, "limit")
				res, err := engine.CoChanges(ctx, file, limit)
				if err != nil {
					return "", err
				}
				return formatCoChanges(res), nil

			default:
				return "", fmt.Errorf("operation must be one of: index, status, search, trace, architecture, dead_code, changes, deps, hierarchy, snippet, tests, co_changes")
			}
		},
	}
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func edgeBreakdown(stats map[graph.Provenance]int) string {
	lsp := stats[graph.ViaLSP]
	name := stats[graph.ViaName]
	amb := stats[graph.ViaAmbiguous]
	if lsp+name+amb == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d precise, %d name, %d ambiguous)", lsp, name, amb)
}

func nodeLabel(n *graph.Node) string {
	return fmt.Sprintf("%s (%s) — %s:%d", n.Name, n.Kind, n.File, n.StartLine)
}

func formatNodes(nodes []*graph.Node) string {
	if len(nodes) == 0 {
		return "No matching symbols."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d symbol(s):\n", len(nodes))
	for _, n := range nodes {
		fmt.Fprintf(&b, "- %s\n", nodeLabel(n))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTrace(res graph.TraceResult, callers bool) string {
	verb := "calls"
	if callers {
		verb = "called by"
	}

	if len(res.Paths) == 0 {
		return fmt.Sprintf("No %s relationships found.", strings.TrimSuffix(verb, " by"))
	}

	ambiguous := false
	var b strings.Builder
	fmt.Fprintf(&b, "%d path(s) (%s):\n", len(res.Paths), verb)
	for _, p := range res.Paths {
		var line strings.Builder
		for i, n := range p.Nodes {
			if i > 0 {
				if i-1 < len(p.Via) && p.Via[i-1] == graph.ViaAmbiguous {
					line.WriteString(" ⇢ ")
					ambiguous = true
				} else {
					line.WriteString(" → ")
				}
			}
			line.WriteString(n.Name)
		}
		last := p.Nodes[len(p.Nodes)-1]
		fmt.Fprintf(&b, "- %s   [%s:%d]\n", line.String(), last.File, last.StartLine)
	}
	if ambiguous {
		b.WriteString("\n⇢ = ambiguous name-based hop (install a language server to resolve precisely)")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatDeps(d graph.DepsResult) string {
	if len(d.DependsOn)+len(d.DependedBy)+len(d.External) == 0 {
		return fmt.Sprintf("Module %q has no recorded imports or importers.", d.Module)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Module: %s", d.Module)
	writeDepList(&b, "Depends on (local)", d.DependsOn)
	writeDepList(&b, "Depended on by (local)", d.DependedBy)
	writeDepList(&b, "Transitive (indirect) deps", d.Transitive)
	writeDepList(&b, "External imports", d.External)
	return strings.TrimRight(b.String(), "\n")
}

func writeDepList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n\n%s (%d):\n", title, len(items))
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
}

func formatHierarchy(h graph.HierarchyResult) string {
	if h.Type == nil {
		return "Type not found."
	}
	total := len(h.Extends) + len(h.Subtypes) + len(h.Implements) + len(h.Implementers)
	if total == 0 {
		return fmt.Sprintf("%s\n(no recorded type-hierarchy relationships)", nodeLabel(h.Type))
	}
	var b strings.Builder
	fmt.Fprint(&b, nodeLabel(h.Type))
	writeNodeList(&b, "Extends", h.Extends)
	writeNodeList(&b, "Subtypes (extended/embedded by)", h.Subtypes)
	writeNodeList(&b, "Implements", h.Implements)
	writeNodeList(&b, "Implemented by", h.Implementers)
	return strings.TrimRight(b.String(), "\n")
}

func writeNodeList(b *strings.Builder, title string, nodes []*graph.Node) {
	if len(nodes) == 0 {
		return
	}
	fmt.Fprintf(b, "\n\n%s (%d):\n", title, len(nodes))
	for _, n := range nodes {
		fmt.Fprintf(b, "- %s\n", nodeLabel(n))
	}
}

func formatTests(res graph.TestsResult) string {
	if res.Symbol == nil {
		return "Symbol not found."
	}
	if len(res.TestedBy) == 0 && len(res.Covers) == 0 {
		return fmt.Sprintf("%s\n(no test relationships detected — direct calls into/out of test files only)", nodeLabel(res.Symbol))
	}
	var b strings.Builder
	fmt.Fprint(&b, nodeLabel(res.Symbol))
	writeNodeList(&b, "Tested by", res.TestedBy)
	writeNodeList(&b, "Covers", res.Covers)
	return strings.TrimRight(b.String(), "\n")
}

func formatCoChanges(res graph.CoChangesResult) string {
	if res.Commits == 0 {
		return fmt.Sprintf("No git history found touching %q (or not a git repo).", res.File)
	}
	if len(res.Related) == 0 {
		return fmt.Sprintf("%s changed in %d commit(s), always alone.", res.File, res.Commits)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Files changing with %s (across %d commit(s)):\n", res.File, res.Commits)
	for _, c := range res.Related {
		fmt.Fprintf(&b, "- %s (%d×)\n", c.File, c.Count)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatChanges(changes graph.Changes) string {
	if len(changes.Files) == 0 {
		return "No uncommitted changes."
	}
	var b strings.Builder
	for _, f := range changes.Files {
		fmt.Fprintf(&b, "%s (%s):\n", f.File, f.Kind)
		if len(f.Nodes) == 0 {
			b.WriteString("  (no tracked definitions affected)\n")
			continue
		}
		for _, n := range f.Nodes {
			fmt.Fprintf(&b, "  - %s\n", nodeLabel(n))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatArch(arch graph.Arch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Files: %d  Definitions: %d  Call edges: %d\n\n", arch.TotalFiles, arch.TotalNodes, arch.TotalEdges)

	b.WriteString("Languages:\n")
	for _, l := range arch.Languages {
		fmt.Fprintf(&b, "- %s: %d files, %d defs\n", l.Lang, l.Files, l.Nodes)
	}

	if len(arch.Layers) > 0 {
		b.WriteString("\nLayers (top-level):\n")
		for _, l := range arch.Layers {
			fmt.Fprintf(&b, "- %s/: %d files, %d defs\n", l.Path, l.Files, l.Nodes)
		}
	}

	if len(arch.Modules) > 0 {
		b.WriteString("\nModules (top by size):\n")
		for _, m := range arch.Modules {
			fmt.Fprintf(&b, "- %s: %d files, %d defs\n", m.Path, m.Files, m.Nodes)
		}
	}

	if len(arch.ModuleDeps) > 0 {
		b.WriteString("\nMost-depended-on modules:\n")
		for _, m := range arch.ModuleDeps {
			fmt.Fprintf(&b, "- %s: %d dependents, %d dependencies\n", m.Module, m.DependedBy, m.DependsOn)
		}
	}

	if len(arch.EntryPoints) > 0 {
		b.WriteString("\nEntry points:\n")
		for _, n := range arch.EntryPoints {
			fmt.Fprintf(&b, "- %s\n", nodeLabel(n))
		}
	}

	if len(arch.Hotspots) > 0 {
		b.WriteString("\nHotspots (most connected):\n")
		for _, h := range arch.Hotspots {
			fmt.Fprintf(&b, "- %s — %d callers, %d callees\n", nodeLabel(h.Node), h.Callers, h.Callees)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}
