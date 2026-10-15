package lsp

import (
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

const fallbackAdvice = "Use available grep/glob/read or code_graph tools; verify changes with relevant compiler/tests. Do not repeatedly retry this unavailable LSP query."

func operationCapability(c lsp.ServerCapabilities, operation string) any {
	switch operation {
	case "goToDefinition":
		return c.DefinitionProvider
	case "goToTypeDefinition":
		return c.TypeDefinitionProvider
	case "findReferences":
		return c.ReferencesProvider
	case "goToImplementation":
		return c.ImplementationProvider
	case "hover":
		return c.HoverProvider
	case "incomingCalls", "outgoingCalls":
		return c.CallHierarchyProvider
	case "documentSymbol":
		return c.DocumentSymbolProvider
	case "workspaceSymbol":
		return c.WorkspaceSymbolProvider
	case "previewRename":
		return c.RenameProvider
	case "codeActions":
		return c.CodeActionProvider
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

func serverStatus(manager *lsp.Manager, args map[string]any) (tool.Result, error) {
	path, _ := args["file_path"].(string)
	if strings.TrimSpace(path) != "" {
		path, err := resolveExistingFile(manager.WorkingDir(), path)
		if err != nil {
			return tool.Result{}, err
		}
		server := manager.FindServer(path)
		if server == nil {
			return tool.Text("LSP unavailable for this file. " + fallbackAdvice), nil
		}
		content := "Detected server: " + server.Name
		if session, ok := manager.ActiveSession(path); ok {
			content += fmt.Sprintf("; running; analyzing=%t\nSupported operations:", session.Analyzing())
			for _, op := range []string{"goToDefinition", "goToTypeDefinition", "findReferences", "goToImplementation", "hover", "documentSymbol", "workspaceSymbol", "incomingCalls", "outgoingCalls", "previewRename", "codeActions"} {
				if requireCapability(session.Capabilities(), op) == nil {
					content += " " + op
				}
			}
		} else {
			content += "; not running. Capabilities unknown until first query; detection does not guarantee successful startup."
		}
		return tool.Text(content), nil
	}
	projects := manager.Projects()
	if len(projects) == 0 {
		return tool.Text("No LSP servers detected in workspace. " + fallbackAdvice), nil
	}
	var content strings.Builder
	content.WriteString("Detected language servers (not started by status; capabilities checked when queried):\n")
	for _, project := range projects {
		fmt.Fprintf(&content, "- %s: %s\n", project.Server.Name, relativePath(manager.WorkingDir(), project.Dir))
	}
	return tool.Text(content.String()), nil
}

// Agent positions and edit preconditions refer to disk, whereas an editor may
// have an unsaved buffer. Never silently query different text at that position.
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
		return fmt.Errorf("%s has unsaved editor changes; LSP positions differ from disk. Save the buffer before querying it; use read/grep for disk content", path)
	}
	return nil
}
