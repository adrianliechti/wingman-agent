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
			"Language-server intelligence: precise, live symbol info (exact binding, types, diagnostics). Discover files/symbols with grep/glob first, then use this for accuracy. For whole-repo structure or multi-hop call/dependency traversal (or when no server is installed), use `code_graph`.",
			"Operations (positions are 1-based, as in read/grep output):",
			"- goToDefinition / findReferences / goToImplementation / hover: need `file_path`+`line`+`column`.",
			"- prepareCallHierarchy / incomingCalls / outgoingCalls: callers/callees at `file_path`+`line`+`column`.",
			"- documentSymbol `file_path`: symbols in one file.",
			"- workspaceSymbol `query`: symbols across the repo.",
			"- diagnostics: errors for `file_path`, or the whole workspace if omitted.",
			"- workspaceDiagnostics: all workspace diagnostics (may be slow).",
			"Errors if no language server is configured for the file type.",
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
					"description": "Which operation to run.",
				},
				"file_path": map[string]any{
					"type":        "string",
					"description": "Target file. Required for position ops and documentSymbol; optional for diagnostics.",
				},
				"line": map[string]any{
					"type":        "integer",
					"description": "1-based line (as in read/grep). Position ops.",
				},
				"column": map[string]any{
					"type":        "integer",
					"description": "1-based column. Position ops.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "workspaceSymbol: symbol name filter (empty lists broadly).",
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
