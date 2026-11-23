package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

const fallbackAdvice = "Use grep/glob/read instead; do not retry this LSP query."

var operations = []string{
	"goToDefinition",
	"findReferences",
	"hover",
	"documentSymbol",
	"workspaceSymbol",
	"goToImplementation",
	"prepareCallHierarchy",
	"incomingCalls",
	"outgoingCalls",
}

func NewTools(service *language.Service) []tool.Tool {
	if service == nil {
		return nil
	}
	return []tool.Tool{lspTool(service)}
}

func lspTool(service *language.Service) tool.Tool {
	return tool.Tool{
		Name: "lsp",
		Description: strings.Join([]string{
			"Query a language server for code intelligence. Read-only. Prefer it to text search for same-named symbols and references through aliases.",
			"",
			"Operations:",
			"- goToDefinition: where a symbol is defined",
			"- findReferences: every reference to a symbol",
			"- hover: type and documentation of a symbol",
			"- documentSymbol: the symbols of a file",
			"- workspaceSymbol: symbols matching query across the workspace; filePath is optional",
			"- goToImplementation: implementations of an interface or abstract method",
			"- prepareCallHierarchy: the function or method at a position",
			"- incomingCalls: callers of the function at a position",
			"- outgoingCalls: functions called by the function at a position",
			"",
			"filePath selects the language server. Operations on a symbol take line and character, or symbol to find it by name in the file. Results include source lines.",
			"A server starts on first use and must be installed; without one the call fails and grep is the fallback.",
		}, "\n"),
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type":        "string",
					"enum":        operations,
					"description": "The LSP operation to perform.",
				},
				"filePath": map[string]any{
					"type":        "string",
					"description": "Absolute or workspace-relative path of the file to operate on. workspaceSymbol without it searches every language in the workspace.",
				},
				"line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based line, as shown in editors.",
				},
				"character": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based character offset in the line, as shown in editors. Requires line.",
				},
				"symbol": map[string]any{
					"type":        "string",
					"description": "Alternative to line and character: the symbol's name in the file (prefer Type.method). With line, the occurrence on that line. Ambiguity returns candidates.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "workspaceSymbol only: the name or partial name to search for.",
				},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			content, err := run(ctx, service, args)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Text(content), nil
		},
	}
}

func run(ctx context.Context, service *language.Service, args map[string]any) (string, error) {
	manager := service.Manager()
	for _, key := range []string{"operation", "filePath", "symbol", "query"} {
		if raw, exists := args[key]; exists {
			if _, ok := raw.(string); !ok {
				return "", fmt.Errorf("%s must be a string", key)
			}
		}
	}
	operation, _ := args["operation"].(string)
	if !slices.Contains(operations, operation) {
		return "", fmt.Errorf("unknown LSP operation %q; use one of %s", operation, strings.Join(operations, ", "))
	}
	if path, _ := args["filePath"].(string); operation == "workspaceSymbol" && strings.TrimSpace(path) == "" {
		query, _ := args["query"].(string)
		symbols, err := service.WorkspaceSymbols(ctx, query)
		if err != nil {
			return "", failed(operation, err)
		}
		return formatWorkspaceSymbols(symbols, manager.WorkingDir()), nil
	}
	path, err := requiredFileArg(manager.WorkingDir(), args)
	if err != nil {
		return "", err
	}
	var position lsp.Position
	var lookup string
	if operation != "documentSymbol" && operation != "workspaceSymbol" {
		if position, lookup, err = resolvePosition(path, args); err != nil {
			return "", err
		}
	}

	session, uri, err := openFile(ctx, manager, path)
	if err != nil {
		return "", err
	}
	if err := requireCapability(session.Capabilities(), operation); err != nil {
		return "", err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, session.RequestTimeout())
	defer cancel()
	content, err := query(ctx, session, manager.WorkingDir(), operation, path, uri, position, lookup, args)
	switch {
	case parent.Err() != nil:
		return "", parent.Err()
	case ctx.Err() != nil:
		return "", fmt.Errorf("the language server did not answer within %s; it may still be indexing. Try again later or use grep", session.RequestTimeout())
	case err != nil:
		return "", err
	}
	if session.Analyzing() {
		content = "The language server is still indexing; results may be incomplete.\n\n" + content
	}
	return content, nil
}

func query(ctx context.Context, session *lsp.Session, root, operation, path, uri string, position lsp.Position, lookup string, args map[string]any) (string, error) {
	var err error
	if lookup != "" {
		line, _, _ := optionalPositiveInt(args, "line")
		if position, err = symbolPosition(ctx, session, uri, lookup, line); err != nil {
			return "", err
		}
	}

	line, character := int(position.Line), int(position.Character)
	var content string
	switch operation {
	case "goToDefinition":
		locations, err := session.DefinitionLocations(ctx, uri, line, character)
		if err != nil {
			return "", failed(operation, err)
		}
		content = formatDefinitions(locations, root)
	case "findReferences":
		locations, err := session.ReferenceLocations(ctx, uri, line, character, true)
		if err != nil {
			return "", failed(operation, err)
		}
		content = formatLocations("References", locations, root)
	case "goToImplementation":
		locations, err := session.ImplementationLocations(ctx, uri, line, character)
		if err != nil {
			return "", failed(operation, err)
		}
		content = formatLocations("Implementations", locations, root)
	case "hover":
		hover, err := session.Hover(ctx, uri, line, character)
		if err != nil {
			return "", failed(operation, err)
		}
		if content = language.HoverText(hover); content == "" {
			content = "No hover information available"
		}
	case "documentSymbol":
		result, err := session.DocumentSymbols(ctx, uri)
		if err != nil {
			return "", failed(operation, err)
		}
		documentSymbols, symbols := language.DocumentSymbolsFromProtocol(result)
		if len(symbols) > 0 {
			content = formatSymbolInformation(symbols, root)
		} else {
			content = formatDocumentSymbols(documentSymbols, path, root)
		}
	case "workspaceSymbol":
		query, _ := args["query"].(string)
		symbols, err := session.WorkspaceSymbols(ctx, query)
		if err != nil {
			return "", failed(operation, err)
		}
		content = formatWorkspaceSymbols(symbols, root)
	case "prepareCallHierarchy", "incomingCalls", "outgoingCalls":
		items, err := session.PrepareCallHierarchy(ctx, uri, line, character)
		if err != nil {
			return "", failed(operation, err)
		}
		switch {
		case operation == "prepareCallHierarchy":
			content = formatCallItems(items, root)
		case len(items) == 0:
			content = "No function or method found at this position"
		case len(items) > 1:
			return "", fmt.Errorf("ambiguous call hierarchy (%d items); choose a more specific position", len(items))
		case operation == "incomingCalls":
			calls, err := session.IncomingCalls(ctx, items[0])
			if err != nil {
				return "", failed(operation, err)
			}
			content = formatIncomingCalls(calls, root)
		default:
			calls, err := session.OutgoingCalls(ctx, items[0])
			if err != nil {
				return "", failed(operation, err)
			}
			content = formatOutgoingCalls(calls, root)
		}
	}
	return content, nil
}

func failed(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s failed: %w. %s", operation, err, fallbackAdvice)
}

func operationCapability(c lsp.ServerCapabilities, operation string) any {
	switch operation {
	case "goToDefinition":
		return c.DefinitionProvider
	case "findReferences":
		return c.ReferencesProvider
	case "goToImplementation":
		return c.ImplementationProvider
	case "hover":
		return c.HoverProvider
	case "prepareCallHierarchy", "incomingCalls", "outgoingCalls":
		return c.CallHierarchyProvider
	case "documentSymbol":
		return c.DocumentSymbolProvider
	case "workspaceSymbol":
		return c.WorkspaceSymbolProvider
	default:
		return nil
	}
}

func requireCapability(c lsp.ServerCapabilities, operation string) error {
	if !lsp.CapabilityEnabled(operationCapability(c, operation)) {
		return fmt.Errorf("LSP operation %s is unsupported by this server. %s", operation, fallbackAdvice)
	}
	return nil
}

func resolvePosition(path string, args map[string]any) (lsp.Position, string, error) {
	line, hasLine, err := optionalPositiveInt(args, "line")
	if err != nil {
		return lsp.Position{}, "", err
	}
	character, hasCharacter, err := optionalPositiveInt(args, "character")
	if err != nil {
		return lsp.Position{}, "", err
	}

	symbol, _ := args["symbol"].(string)
	symbol = strings.TrimSpace(symbol)

	switch {
	case hasCharacter && !hasLine:
		return lsp.Position{}, "", fmt.Errorf("line must be a positive 1-based integer when character is supplied")
	case hasLine && hasCharacter:
		pos, err := positionFromDisplay(path, line, character)
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
		return lsp.Position{}, "", fmt.Errorf("character must be a positive 1-based integer (or pass symbol instead)")
	default:
		return lsp.Position{}, "", fmt.Errorf("position required: pass line and character, or symbol (optionally with line)")
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

func requiredFileArg(workingDir string, args map[string]any) (string, error) {
	path, _ := args["filePath"].(string)
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("filePath is required")
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err
		}
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

// Agent positions refer to disk, whereas an editor may have an unsaved
// buffer. Never silently query different text at that position.
func requireSavedContent(session *lsp.Session, path string) error {
	buffer, ok := session.DocumentContent(path)
	if !ok {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if buffer != string(data) {
		return fmt.Errorf("the editor buffer for %s differs from disk. Save or reload it before querying LSP, or use read/grep for disk content", path)
	}
	return nil
}
