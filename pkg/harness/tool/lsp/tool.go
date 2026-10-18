package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func NewTools(service *language.Service) []tool.Tool {
	if service == nil {
		return nil
	}
	return []tool.Tool{lspTool(service)}
}

func lspTool(service *language.Service) tool.Tool {
	manager := service.Manager()
	return tool.Tool{
		Name: "lsp",
		Description: strings.Join([]string{
			"Read-only language-server queries: exact definitions, types, references, implementations, calls, diagnostics and refactoring previews. Prefer for aliases and same-name symbols; use code_graph for multi-hop paths.",
			"Call known files/symbols directly; no status preflight. Servers/capabilities may be unavailable.",
			"Navigation, hover, calls and previewRename require file_path plus symbol or line+column. Results include source lines; definitions include snippets.",
			"findReferences includes non-call uses; incomingCalls/outgoingCalls report calls. Rename/code-action previews never modify files or execute commands; apply through edit.",
			"Missing/pending diagnostics do not prove clean code. Follow pagination for exhaustive results.",
		}, "\n"),
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "string",
					"enum": []string{
						"status",
						"diagnostics",
						"goToDefinition",
						"goToTypeDefinition",
						"findReferences",
						"hover",
						"documentSymbol",
						"workspaceSymbol",
						"goToImplementation",
						"incomingCalls",
						"outgoingCalls",
						"previewRename",
						"codeActions",
					},
					"description": "status reports servers/capabilities without starting them; documentSymbol lists file symbols; workspaceSymbol searches repo symbols.",
				},
				"file_path": map[string]any{
					"type":        "string",
					"description": "Required except for workspaceSymbol, diagnostics and status. diagnostics: omit for a bounded workspace scan; status: supply for file capabilities.",
				},
				"line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based line; pair with column or constrain symbol to this line.",
				},
				"column": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based Unicode character column, not bytes. Requires line.",
				},
				"symbol": map[string]any{
					"type":        "string",
					"description": "Alternative to line+column: declaration name (prefer Type.method); unqualified names can select a unique occurrence. Ambiguity returns candidates.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "workspaceSymbol: name filter; empty lists broadly.",
				},
				"offset":              map[string]any{"type": "integer", "minimum": 0, "description": "0-based pagination offset; default 0."},
				"limit":               map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Results per page; default 100."},
				"new_name":            map[string]any{"type": "string", "description": "previewRename: proposed new symbol name."},
				"action_title":        map[string]any{"type": "string", "description": "codeActions: omit to list actions for file/optional position; exact title to preview one."},
				"output_mode":         map[string]any{"type": "string", "enum": []string{"locations", "count"}, "description": "findReferences: source locations (default) or counts per file; count pagination pages files."},
				"include_declaration": map[string]any{"type": "boolean", "description": "findReferences: default true; false for uses only."},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (result tool.Result, err error) {
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			defer func() {
				if ctx.Err() != nil {
					result, err = tool.Result{}, ctx.Err()
				}
			}()
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			for _, key := range []string{"operation", "file_path", "symbol", "query", "new_name", "action_title", "output_mode"} {
				if raw, exists := args[key]; exists {
					if _, ok := raw.(string); !ok {
						return tool.Result{}, fmt.Errorf("%s must be a string", key)
					}
				}
			}
			if raw, exists := args["output_mode"]; exists {
				if mode, _ := raw.(string); mode != "locations" && mode != "count" {
					return tool.Result{}, fmt.Errorf("output_mode must be locations or count")
				}
			}
			if raw, exists := args["include_declaration"]; exists {
				if _, ok := raw.(bool); !ok {
					return tool.Result{}, fmt.Errorf("include_declaration must be boolean")
				}
			}
			for _, key := range []string{"line", "column"} {
				if _, _, err := optionalPositiveInt(args, key); err != nil {
					return tool.Result{}, err
				}
			}
			if _, column := args["column"]; column {
				if _, line := args["line"]; !line {
					return tool.Result{}, fmt.Errorf("line must be a positive 1-based integer when column is supplied")
				}
			}
			operation, _ := args["operation"].(string)
			page, err := parsePage(args)
			if err != nil {
				return tool.Result{}, err
			}

			runPosition := func(fn func(session *lsp.Session, uri string, line, column int) (string, error)) (string, error) {
				path, err := requiredFileArg(manager.WorkingDir(), args, "file_path")
				if err != nil {
					return "", err
				}

				pos, lookup, err := resolvePosition(path, args)
				if err != nil {
					return "", err
				}

				session, uri, err := openFile(ctx, manager, path)
				if err != nil {
					return "", err
				}
				if err := requireCapability(session.Capabilities(), operation); err != nil {
					return "", err
				}

				if lookup != "" {
					line, _, _ := optionalPositiveInt(args, "line")
					pos, err = symbolPosition(ctx, session, uri, lookup, line)
					if err != nil {
						return "", err
					}
				}

				content, err := fn(session, uri, int(pos.Line), int(pos.Character))
				if err != nil {
					return content, fmt.Errorf("%s failed: %w. %s", operation, err, fallbackAdvice)
				}
				return content, nil
			}

			switch operation {
			case "status":
				return serverStatus(manager, args)
			case "codeActions":
				return previewCodeActions(ctx, manager, args, page)
			case "previewRename":
				started := time.Now()
				newName, _ := args["new_name"].(string)
				if strings.TrimSpace(newName) == "" {
					return tool.Result{}, fmt.Errorf("new_name is required")
				}
				var preview tool.Result
				_, err := runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					edit, err := s.Rename(ctx, uri, line, column, newName)
					if err != nil {
						return "", err
					}
					preview, err = workspaceEditPreview(manager, edit, started)
					return preview.Content, err
				})
				return preview, err
			case "diagnostics":
				path, _ := args["file_path"].(string)
				if strings.TrimSpace(path) == "" {
					if len(manager.DetectServers()) == 0 {
						return tool.Result{}, fmt.Errorf("no LSP servers detected in workspace; diagnostics unavailable. %s", fallbackAdvice)
					}
					return tool.Text(formatWorkspaceDiagnostics(service.Diagnostics(ctx), manager.WorkingDir())), nil
				}

				path, err := resolveExistingFile(manager.WorkingDir(), path)
				if err != nil {
					return tool.Result{}, err
				}

				session, uri, err := openFile(ctx, manager, path)
				if err != nil {
					return tool.Result{}, err
				}

				raw, known := session.WaitForDiagnostics(ctx, uri, 3*time.Second)
				if !known {
					return tool.Text("No diagnostics data: the language server did not report results for this file (it may still be analyzing or not support diagnostics). Do not treat this as a clean result."), nil
				}
				values := language.DiagnosticsFromProtocol(raw)
				if len(values) == 0 {
					return tool.Text("No diagnostics found"), nil
				}
				return tool.Text(language.FormatDiagnostics(values, path, manager.WorkingDir())), nil
			case "workspaceSymbol":
				query, _ := args["query"].(string)
				symbols, workspaceSymbols, err := service.WorkspaceSymbols(ctx, query)
				if err != nil && len(symbols)+len(workspaceSymbols) == 0 {
					return tool.Result{}, fmt.Errorf("workspace symbol search unavailable/incomplete: %w. %s", err, fallbackAdvice)
				}
				for _, symbol := range symbols {
					location := symbol.Location
					converted := lsp.WorkspaceSymbol{Location: &location}
					converted.Name, converted.Kind = symbol.Name, symbol.Kind
					workspaceSymbols = append(workspaceSymbols, converted)
				}
				if len(workspaceSymbols) > 0 {
					content := formatWorkspaceSymbols(workspaceSymbols, manager.WorkingDir(), page)
					if err != nil {
						content = "Partial workspace results: " + err.Error() + ". " + fallbackAdvice + "\n\n" + content
					}
					return tool.Text(content), nil
				}
				return tool.Text("No symbols found"), nil
			case "documentSymbol":
				path, err := requiredFileArg(manager.WorkingDir(), args, "file_path")
				if err != nil {
					return tool.Result{}, err
				}
				session, uri, err := openFile(ctx, manager, path)
				if err != nil {
					return tool.Result{}, err
				}
				if err := requireCapability(session.Capabilities(), "documentSymbol"); err != nil {
					return tool.Result{}, err
				}
				result, err := session.DocumentSymbols(ctx, uri)
				documentSymbols, symbols := language.DocumentSymbolsFromProtocol(result)
				if err != nil {
					return tool.Result{}, err
				}
				if len(symbols) > 0 {
					return tool.Text(formatSymbolInformation(symbols, manager.WorkingDir(), page)), nil
				}
				if len(documentSymbols) > 0 {
					return tool.Text(formatDocumentSymbolPage(documentSymbols, path, manager.WorkingDir(), page)), nil
				}
				return tool.Text("No symbols found"), nil
			case "goToDefinition", "goToTypeDefinition":
				content, err := runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					var locations []lsp.Location
					var err error
					if operation == "goToTypeDefinition" {
						locations, err = s.TypeDefinitionLocations(ctx, uri, line, column)
					} else {
						locations, err = s.DefinitionLocations(ctx, uri, line, column)
					}
					if err != nil || len(locations) == 0 {
						return "No definition found", err
					}
					return formatDefinitions(locations, manager.WorkingDir(), page), nil
				})
				return tool.Text(content), err
			case "findReferences":
				mode := "locations"
				if raw, exists := args["output_mode"]; exists {
					mode, _ = raw.(string)
				}
				include := true
				if raw, exists := args["include_declaration"]; exists {
					include, _ = raw.(bool)
				}
				content, err := runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					locations, err := s.ReferenceLocations(ctx, uri, line, column, include)
					if err == nil && mode == "count" {
						return formatReferenceCounts(locations, manager.WorkingDir(), include, page), nil
					}
					if err != nil || len(locations) == 0 {
						return "No references found", err
					}
					return formatLocations("References", locations, manager.WorkingDir(), page), nil
				})
				return tool.Text(content), err
			case "hover":
				content, err := runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					hover, err := s.Hover(ctx, uri, line, column)
					content := language.HoverText(hover)
					if err == nil && content == "" {
						content = "No hover information available"
					}
					return content, err
				})
				return tool.Text(content), err
			case "goToImplementation":
				content, err := runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					locations, err := s.ImplementationLocations(ctx, uri, line, column)
					if err != nil || len(locations) == 0 {
						return "No implementations found", err
					}
					return formatLocations("Implementations", locations, manager.WorkingDir(), page), nil
				})
				return tool.Text(content), err
			case "incomingCalls", "outgoingCalls":
				content, err := runPosition(func(s *lsp.Session, uri string, line, column int) (string, error) {
					items, err := s.PrepareCallHierarchy(ctx, uri, line, column)
					if err != nil || len(items) == 0 {
						return "No call hierarchy item found at this position", err
					}
					if len(items) > 1 {
						return "", fmt.Errorf("ambiguous call hierarchy (%d items); choose a more specific position", len(items))
					}
					var content string
					if operation == "incomingCalls" {
						calls, err := s.IncomingCalls(ctx, items[0])
						if err != nil || len(calls) == 0 {
							return "No incoming calls found", err
						}
						content = formatIncomingCalls(calls, manager.WorkingDir(), page)
					} else {
						calls, err := s.OutgoingCalls(ctx, items[0])
						if err != nil || len(calls) == 0 {
							return "No outgoing calls found", err
						}
						content = formatOutgoingCalls(calls, manager.WorkingDir(), page)
					}
					return content, nil
				})
				return tool.Text(content), err
			default:
				return tool.Result{}, fmt.Errorf("unknown LSP operation %q; see the operation enum", operation)
			}
		},
	}
}

func resolvePosition(path string, args map[string]any) (lsp.Position, string, error) {
	line, hasLine, err := optionalPositiveInt(args, "line")
	if err != nil {
		return lsp.Position{}, "", err
	}

	column, hasColumn, err := optionalPositiveInt(args, "column")
	if err != nil {
		return lsp.Position{}, "", err
	}

	symbol, _ := args["symbol"].(string)
	symbol = strings.TrimSpace(symbol)

	switch {
	case hasLine && hasColumn:
		pos, err := positionFromDisplay(path, line, column)
		return pos, "", err
	case symbol != "" && hasLine && strings.Contains(symbol, "."):
		// A qualified declaration need not appear literally in source (Go's
		// "func (Beta) Close", for example). Resolve it semantically, with
		// the line as a constraint, rather than dropping its receiver.
		_, err := positionFromDisplay(path, line, 1)
		return lsp.Position{}, symbol, err
	case symbol != "" && hasLine:
		pos, err := positionOfSymbolOnLine(path, line, symbol)
		return pos, "", err
	case symbol != "":
		return lsp.Position{}, symbol, nil
	case hasLine:
		return lsp.Position{}, "", fmt.Errorf("column must be a positive 1-based integer (or pass symbol instead)")
	case hasColumn:
		return lsp.Position{}, "", fmt.Errorf("line must be a positive 1-based integer (or pass symbol instead)")
	default:
		return lsp.Position{}, "", fmt.Errorf("position required: pass line and column, or symbol (optionally with line)")
	}
}

func optionalPositiveInt(args map[string]any, key string) (int, bool, error) {
	raw, present := args[key]
	if !present {
		return 0, false, nil
	}

	value, ok := tool.IntValue(raw)
	if !ok || value <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive 1-based integer", key)
	}

	return value, true, nil
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
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("path is not a regular file: %s", path)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return "", err
	}
	if !pathInsideWorkspace(realPath, realRoot) {
		return "", fmt.Errorf("path %q resolves outside workspace", path)
	}

	return path, nil
}

func pathInsideWorkspace(path, workingDir string) bool {
	rel, err := filepath.Rel(workingDir, path)
	return err == nil && filepath.IsLocal(rel)
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

func openFile(ctx context.Context, manager *lsp.Manager, path string) (*lsp.Session, string, error) {
	session, err := manager.GetSession(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("%w. %s", err, fallbackAdvice)
	}

	uri, err := session.OpenDocument(ctx, path)
	if err != nil {
		return nil, "", err
	}
	if err := requireSavedContent(session, path); err != nil {
		return nil, "", err
	}

	return session, uri, nil
}
