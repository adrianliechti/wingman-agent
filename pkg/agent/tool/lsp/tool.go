package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func NewTools(manager *lsp.Manager) []tool.Tool {
	return []tool.Tool{lspTool(manager)}
}

func lspTool(manager *lsp.Manager) tool.Tool {
	return tool.Tool{
		Name: "lsp",
		Description: strings.Join([]string{
			"Use Language Server Protocol servers for semantic code intelligence.",
			"Use `grep`/`glob` first to discover candidate files or symbols; use `lsp` when semantic accuracy matters.",
			"For whole-repo structure, cross-language symbol search, or call/dependency graphs — or when no server is installed — use `code_graph` instead. `lsp` is the precise, live layer (exact binding, types, diagnostics); `code_graph` is the always-on structural layer.",
			"Operations: diagnostics, workspaceDiagnostics, goToDefinition, findReferences, hover, documentSymbol, workspaceSymbol, goToImplementation, prepareCallHierarchy, incomingCalls, outgoingCalls.",
			"Position operations require `path`, `line`, and `column`; lines/columns are 1-based, matching editors and `read`/`grep` output.",
			"`documentSymbol` requires `path`; `workspaceSymbol` uses optional `query`; `diagnostics` uses optional `path`; `workspaceDiagnostics` ignores `path` and may be slower.",
			"Returns an error when no language server is configured for the file type.",
		}, "\n"),
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "string",
					"enum": []string{
						"diagnostics",
						"workspaceDiagnostics",
						"goToDefinition",
						"findReferences",
						"hover",
						"documentSymbol",
						"workspaceSymbol",
						"goToImplementation",
						"prepareCallHierarchy",
						"incomingCalls",
						"outgoingCalls",
					},
					"description": "The LSP operation to perform.",
				},
				"file_path": map[string]any{
					"type":        "string",
					"description": "File path. Required for file and position operations; optional for diagnostics; ignored for workspaceDiagnostics/workspaceSymbol.",
				},
				"line": map[string]any{
					"type":        "integer",
					"description": "Line number, 1-based as shown by `read`, `grep`, and editors. Required for position operations.",
				},
				"column": map[string]any{
					"type":        "integer",
					"description": "Column/character offset, 1-based as shown by editors. Required for position operations.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Workspace symbol query. Used only by workspaceSymbol; omit or pass an empty string to list broadly.",
				},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			operation, _ := args["operation"].(string)

			runPosition := func(fn func(session *lsp.Session, uri string, line, column int) (string, error)) (string, error) {
				path, line, column, err := parsePositionArgs(manager.WorkingDir(), args)
				if err != nil {
					return "", err
				}
				session, uri, err := openFile(ctx, manager, path)
				if err != nil {
					return "", err
				}
				return fn(session, uri, line, column)
			}

			switch operation {
			case "diagnostics":
				path, _ := args["file_path"].(string)
				if strings.TrimSpace(path) == "" {
					return manager.WorkspaceDiagnostics(ctx)
				}

				path, err := resolveExistingFile(manager.WorkingDir(), path)
				if err != nil {
					return "", err
				}

				session, uri, err := openFile(ctx, manager, path)
				if err != nil {
					return "", err
				}

				return session.Diagnostics(ctx, uri, path)
			case "workspaceDiagnostics":
				return manager.WorkspaceDiagnostics(ctx)
			case "workspaceSymbol":
				query, _ := args["query"].(string)
				return manager.WorkspaceSymbols(ctx, query)
			case "documentSymbol":
				path, err := requiredFileArg(manager.WorkingDir(), args, "file_path")
				if err != nil {
					return "", err
				}
				session, uri, err := openFile(ctx, manager, path)
				if err != nil {
					return "", err
				}
				return session.DocumentSymbols(ctx, uri, path)
			case "goToDefinition":
				return runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					return s.Definition(ctx, uri, line, column)
				})
			case "findReferences":
				return runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					return s.References(ctx, uri, line, column)
				})
			case "hover":
				return runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					return s.Hover(ctx, uri, line, column)
				})
			case "goToImplementation":
				return runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					return s.Implementation(ctx, uri, line, column)
				})
			case "prepareCallHierarchy":
				return runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					return s.PrepareCallHierarchy(ctx, uri, line, column)
				})
			case "incomingCalls", "outgoingCalls":
				return runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					return s.CallHierarchy(ctx, uri, line, column, operation == "incomingCalls")
				})
			default:
				return "", fmt.Errorf("operation must be one of: diagnostics, workspaceDiagnostics, goToDefinition, findReferences, hover, documentSymbol, workspaceSymbol, goToImplementation, prepareCallHierarchy, incomingCalls, outgoingCalls")
			}
		},
	}
}

func requiredFileArg(workingDir string, args map[string]any, key string) (string, error) {
	path, _ := args[key].(string)
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s is required", key)
	}

	return resolveExistingFile(workingDir, path)
}

func resolveExistingFile(workingDir, path string) (string, error) {
	path = expandHome(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir, path)
	}
	path = filepath.Clean(path)
	workingDir = filepath.Clean(workingDir)

	if !pathInsideWorkspace(path, workingDir) {
		return "", fmt.Errorf("path %q is outside workspace %q", path, workingDir)
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("file not found: %s", path)
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", path)
	}

	return path, nil
}

func pathInsideWorkspace(path, workingDir string) bool {
	cp, cw := path, workingDir
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		cp = strings.ToLower(cp)
		cw = strings.ToLower(cw)
	}
	if cp == cw {
		return true
	}
	return strings.HasPrefix(cp, cw+string(filepath.Separator))
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func parsePositionArgs(workingDir string, args map[string]any) (string, int, int, error) {
	path, err := requiredFileArg(workingDir, args, "file_path")
	if err != nil {
		return "", 0, 0, err
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

func requiredPositiveIntArg(args map[string]any, key string) (int, bool) {
	value, ok := tool.IntArg(args, key)
	return value, ok && value > 0
}
