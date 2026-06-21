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
			"Code knowledge graph (tree-sitter, many languages): definitions and their call/type/import links. Use instead of grep/read loops to find symbols and follow relationships; auto-builds on first use. With a language server, prefer `lsp` for precise definitions/references/types.",
			"Operations (each uses the field in backticks):",
			"- search `query`: definitions by name regex; optional `kind`, `file`.",
			"- trace `symbol`: call paths; `direction` callees (default) or callers; optional `target`.",
			"- find_similar `symbol`: functions resembling it (shared callees + name).",
			"- hierarchy `symbol`: super/sub types and implementers.",
			"- tests `symbol`: tests covering it, or what a test covers.",
			"- snippet `symbol`: its source code.",
			"- deps `file`: module imports/importers; `depth`>1 for transitive.",
			"- co_changes `file`: files historically committed together with it.",
			"- changes: current uncommitted edits mapped to definitions.",
			"- architecture: overview of languages, modules, entry points, hotspots.",
			"- dead_code: callables with no known caller (candidates; misses reflection/exported/cross-language).",
			"- index / status: rebuild the graph / report its state.",
		}, "\n"),
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type":        "string",
					"enum":        []string{"index", "status", "search", "trace", "architecture", "dead_code", "changes", "deps", "hierarchy", "snippet", "tests", "co_changes", "find_similar"},
					"description": "Which operation to run.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "search: name regex (case-insensitive).",
				},
				"symbol": map[string]any{
					"type":        "string",
					"description": "trace/find_similar/hierarchy/tests/snippet: one exact symbol name.",
				},
				"target": map[string]any{
					"type":        "string",
					"description": "trace: optional destination symbol to reach.",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"callees", "callers"},
					"description": "trace: callees (default) or callers.",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"function", "method", "class", "interface", "type", "constructor", "module", "constant", "variable"},
					"description": "search: restrict to a definition kind.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "deps: target module/dir/file. co_changes: exact repo-relative path. search/snippet/tests/find_similar: optional path-substring filter.",
				},
				"depth": map[string]any{
					"type":        "integer",
					"description": "trace path depth (default 8); deps transitive depth (default 1).",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "max results — search (default 50), dead_code (100), co_changes/find_similar (15).",
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

			case "find_similar":
				symbol := strings.TrimSpace(stringArg(args, "symbol"))
				if symbol == "" {
					return "", fmt.Errorf("symbol is required for find_similar")
				}
				limit, _ := tool.IntArg(args, "limit")
				res, err := engine.Similar(ctx, symbol, stringArg(args, "file"), limit)
				if err != nil {
					return "", err
				}
				return formatSimilar(res), nil

			default:
				return "", fmt.Errorf("operation must be one of: index, status, search, trace, architecture, dead_code, changes, deps, hierarchy, snippet, tests, co_changes, find_similar")
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

// maxListItems caps how many entries any single rendered list shows, so a
// high-degree symbol (a popular interface, a widely-imported package) can't
// flood the agent's context. The full count is always reported in the header.
const maxListItems = 40

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
	paths := res.Paths
	if len(paths) > maxListItems {
		paths = paths[:maxListItems]
	}
	for _, p := range paths {
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
	if len(res.Paths) > len(paths) {
		fmt.Fprintf(&b, "  … and %d more path(s); narrow with `target` or a smaller `depth`\n", len(res.Paths)-len(paths))
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
	shown := items
	if len(shown) > maxListItems {
		shown = shown[:maxListItems]
	}
	for _, it := range shown {
		fmt.Fprintf(b, "- %s\n", it)
	}
	if len(items) > len(shown) {
		fmt.Fprintf(b, "  … and %d more\n", len(items)-len(shown))
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
	shown := nodes
	if len(shown) > maxListItems {
		shown = shown[:maxListItems]
	}
	for _, n := range shown {
		fmt.Fprintf(b, "- %s\n", nodeLabel(n))
	}
	if len(nodes) > len(shown) {
		fmt.Fprintf(b, "  … and %d more\n", len(nodes)-len(shown))
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

func formatSimilar(res graph.SimilarResult) string {
	if res.Target == nil {
		return "Symbol not found."
	}
	if len(res.Matches) == 0 {
		return fmt.Sprintf("%s\n(no similar functions found)", nodeLabel(res.Target))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Similar to %s:\n", nodeLabel(res.Target))
	for _, m := range res.Matches {
		fmt.Fprintf(&b, "- %s  (%.2f)\n", nodeLabel(m.Node), m.Score)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatCoChanges(res graph.CoChangesResult) string {
	if res.Commits == 0 {
		return fmt.Sprintf("No git history found touching %q. Pass an exact repo-relative path (not a substring), and check this is a git repo.", res.File)
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
