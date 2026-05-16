package lsp

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func NewTools(manager *lsp.Manager) []tool.Tool {
	return []tool.Tool{
		diagnosticsTool(manager),
		definitionTool(manager),
		referencesTool(manager),
		implementationTool(manager),
		hoverTool(manager),
		symbolsTool(manager),
		hierarchyTool(manager),
	}
}

func diagnosticsTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "get_lsp_diagnostics",
		Description: "Get language-server diagnostics (errors, warnings) for a file or the entire workspace. Use after edits or when investigating compile/type issues. Clean (empty) output means no issues — success. Omit `path` for workspace diagnostics only when needed; it can be slower.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the working directory. Omit for all diagnostics.",
				},
			},
		},
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, _ := args["path"].(string)

			if path == "" {
				return manager.WorkspaceDiagnostics(ctx)
			}

			path = absPath(manager.WorkingDir(), path)

			if _, err := os.Stat(path); os.IsNotExist(err) {
				return "", fmt.Errorf("file not found: %s", path)
			}

			session, err := manager.GetSession(ctx, path)
			if err != nil {
				return "", err
			}

			uri, err := session.OpenDocument(ctx, path)
			if err != nil {
				return "", err
			}

			return session.Diagnostics(ctx, uri, path)
		},
	}
}

func definitionTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "find_lsp_definition",
		Description: "Find the definition of a symbol at a known file position. Use after `grep`/`read` has located the symbol occurrence. Uses 1-based editor line/column positions, matching `read` and `grep` output.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters:  positionParams(),
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, line, column, err := parsePositionArgs(manager.WorkingDir(), args)
			if err != nil {
				return "", err
			}

			session, uri, err := openFile(ctx, manager, path)
			if err != nil {
				return "", err
			}

			return session.Definition(ctx, uri, line, column)
		},
	}
}

func referencesTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "find_lsp_references",
		Description: "Find semantic references to a symbol at a known file position across the workspace. Prefer this over text `grep` when you need rename/call-site accuracy. Uses 1-based editor line/column positions, matching `read` and `grep` output.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters:  positionParams(),
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, line, column, err := parsePositionArgs(manager.WorkingDir(), args)
			if err != nil {
				return "", err
			}

			session, uri, err := openFile(ctx, manager, path)
			if err != nil {
				return "", err
			}

			return session.References(ctx, uri, line, column)
		},
	}
}

func implementationTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "find_lsp_implementation",
		Description: "Find implementations of an interface or abstract method at a known file position. Use after locating the interface/member with `grep`, `find_lsp_symbols`, or `read`. Uses 1-based editor line/column positions.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters:  positionParams(),
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, line, column, err := parsePositionArgs(manager.WorkingDir(), args)
			if err != nil {
				return "", err
			}

			session, uri, err := openFile(ctx, manager, path)
			if err != nil {
				return "", err
			}

			return session.Implementation(ctx, uri, line, column)
		},
	}
}

func hoverTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "get_lsp_hover",
		Description: "Get hover information (type, signature, documentation) for a symbol at a known file position. Use when local source context is insufficient. Uses 1-based editor line/column positions.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters:  positionParams(),
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, line, column, err := parsePositionArgs(manager.WorkingDir(), args)
			if err != nil {
				return "", err
			}

			session, uri, err := openFile(ctx, manager, path)
			if err != nil {
				return "", err
			}

			return session.Hover(ctx, uri, line, column)
		},
	}
}

func symbolsTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "find_lsp_symbols",
		Description: "Find symbols. With `path`, returns that file's outline (functions, classes, variables) and ignores `query`. Without `path`, searches workspace symbols by `query`. Use for symbol-name discovery; use `grep` for arbitrary text/config/string searches.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the working directory. If provided, returns symbols in that file.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Search query for workspace-wide symbol search. Used when path is omitted.",
				},
			},
		},
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, _ := args["path"].(string)
			query, _ := args["query"].(string)

			if path == "" {
				return manager.WorkspaceSymbols(ctx, query)
			}

			path = absPath(manager.WorkingDir(), path)

			if _, err := os.Stat(path); os.IsNotExist(err) {
				return "", fmt.Errorf("file not found: %s", path)
			}

			session, err := manager.GetSession(ctx, path)
			if err != nil {
				return "", err
			}

			uri, err := session.OpenDocument(ctx, path)
			if err != nil {
				return "", err
			}

			return session.DocumentSymbols(ctx, uri, path)
		},
	}
}

func hierarchyTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name:        "find_lsp_hierarchy",
		Description: "Trace semantic calls for a function/method at a known position. `direction=incoming` returns WHO CALLS this; `direction=outgoing` returns WHAT THIS CALLS. Position must be on the function name. Use after `grep`/`read` identifies the exact function. Uses 1-based editor line/column positions.",
		Effect:      tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the working directory.",
				},
				"line": map[string]any{
					"type":        "integer",
					"description": "Line number, 1-based as shown by `read`, `grep`, and editors.",
				},
				"column": map[string]any{
					"type":        "integer",
					"description": "Column number, 1-based as shown by editors.",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"incoming", "outgoing"},
					"description": "incoming = who calls this; outgoing = what this calls.",
				},
			},
			"required": []string{"path", "line", "column", "direction"},
		},
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			path, line, column, err := parsePositionArgs(manager.WorkingDir(), args)
			if err != nil {
				return "", err
			}

			direction, _ := args["direction"].(string)
			if direction != "incoming" && direction != "outgoing" {
				return "", fmt.Errorf("direction must be 'incoming' or 'outgoing'")
			}

			session, uri, err := openFile(ctx, manager, path)
			if err != nil {
				return "", err
			}

			return session.CallHierarchy(ctx, uri, line, column, direction == "incoming")
		},
	}
}

func positionParams() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to the working directory.",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "Line number, 1-based as shown by `read`, `grep`, and editors.",
			},
			"column": map[string]any{
				"type":        "integer",
				"description": "Column number, 1-based as shown by editors.",
			},
		},
		"required": []string{"path", "line", "column"},
	}
}

func parsePositionArgs(workingDir string, args map[string]any) (string, int, int, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", 0, 0, fmt.Errorf("path is required")
	}

	path = absPath(workingDir, path)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", 0, 0, fmt.Errorf("file not found: %s", path)
	}

	line, ok := requiredPositiveIntArg(args, "line")
	if !ok {
		return "", 0, 0, fmt.Errorf("line must be a positive 1-based integer")
	}

	column, ok := requiredPositiveIntArg(args, "column")
	if !ok {
		return "", 0, 0, fmt.Errorf("column must be a positive 1-based integer")
	}

	return path, line - 1, column - 1, nil
}

func openFile(ctx context.Context, manager *lsp.Manager, path string) (*lsp.Session, string, error) {
	session, err := manager.GetSession(ctx, path)
	if err != nil {
		return nil, "", err
	}

	uri, err := session.OpenDocument(ctx, path)
	if err != nil {
		return nil, "", err
	}

	return session, uri, nil
}

func absPath(workingDir, path string) string {
	if !filepath.IsAbs(path) {
		return filepath.Join(workingDir, path)
	}
	return path
}

func requiredPositiveIntArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case int:
		return v, v > 0
	case float64:
		if v > float64(math.MaxInt) || v < float64(math.MinInt) {
			return 0, false
		}
		iv := int(v)
		return iv, iv > 0
	case int64:
		if v > int64(math.MaxInt) || v < int64(math.MinInt) {
			return 0, false
		}
		iv := int(v)
		return iv, iv > 0
	default:
		return 0, false
	}
}
