package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

type fixtureManagedTools map[string]string

func (m fixtureManagedTools) Resolve(command string) string { return m[command] }

// A real subprocess exercises initialization, capability detection, failed
// RPCs and partial workspace results through the production session manager.
func TestLSPFixtureServer(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "lsp-fixture" {
		return
	}
	mode, logPath := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	reader := bufio.NewReader(os.Stdin)
	for {
		length := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				os.Exit(0)
			}
			if line == "\r\n" {
				break
			}
			fmt.Sscanf(line, "Content-Length: %d", &length)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			os.Exit(0)
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &request) != nil {
			os.Exit(2)
		}
		if file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
			fmt.Fprintln(file, request.Method)
			file.Close()
		}
		if len(request.ID) == 0 {
			if request.Method == "exit" {
				os.Exit(0)
			}
			continue
		}
		var result any
		var rpcError any
		switch request.Method {
		case "initialize":
			result = map[string]any{"capabilities": map[string]any{"textDocumentSync": 1, "hoverProvider": true, "workspaceSymbolProvider": true, "documentSymbolProvider": true, "codeActionProvider": true}}
		case "textDocument/hover":
			if mode == "fail" {
				rpcError = map[string]any{"code": -32603, "message": "fixture server failed"}
			} else {
				result = map[string]any{"contents": "fixture hover"}
			}
		case "workspace/symbol":
			if mode == "fail" {
				rpcError = map[string]any{"code": -32603, "message": "fixture search failed"}
			} else {
				result = []any{map[string]any{"name": "FoundSymbol", "kind": 12, "location": map[string]any{"uri": "file:///fixture/one.mock", "range": map[string]any{"start": map[string]int{"line": 0, "character": 0}, "end": map[string]int{"line": 0, "character": 1}}}}}
			}
		case "textDocument/documentSymbol":
			result = []any{}
		case "textDocument/codeAction":
			result = []any{map[string]any{"title": "Run command", "kind": "quickfix", "command": map[string]any{"title": "Command", "command": "fixture.mutate"}}}
		case "shutdown":
			result = nil
		default:
			rpcError = map[string]any{"code": -32601, "message": "unexpected method: " + request.Method}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if rpcError != nil {
			response["error"] = rpcError
		} else {
			response["result"] = result
		}
		encoded, _ := json.Marshal(response)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(encoded), encoded)
	}
}

func fixtureService(t *testing.T, modes ...string) (*language.Service, string, []string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var definitions []lsp.ProjectDefinition
	var paths, logs []string
	for i, mode := range modes {
		name := fmt.Sprintf("fixture%d", i)
		dir := filepath.Join(root, name)
		os.MkdirAll(dir, 0700)
		os.WriteFile(filepath.Join(dir, name+".marker"), nil, 0600)
		path := filepath.Join(dir, "one."+name)
		os.WriteFile(path, []byte("value\n"), 0600)
		paths = append(paths, path)
		log := filepath.Join(t.TempDir(), "rpc.log")
		logs = append(logs, log)
		definitions = append(definitions, lsp.ProjectDefinition{Name: name, Markers: []string{name + ".marker"}, Servers: []lsp.Server{{Name: name, Command: name, Args: []string{"-test.run=^TestLSPFixtureServer$", "--", "lsp-fixture", mode, log}, Languages: []string{name}, LanguageID: "fixture"}}})
	}
	encoded, _ := json.Marshal(definitions)
	registry, err := lsp.ParseRegistry(encoded)
	if err != nil {
		t.Fatal(err)
	}
	managed := fixtureManagedTools{}
	for i := range modes {
		managed[fmt.Sprintf("fixture%d", i)] = executable
	}
	service := language.New(root, filepath.Join(t.TempDir(), "graph.json"), lsp.WithManagedTools(managed), lsp.WithRegistry(registry))
	t.Cleanup(service.Close)
	return service, paths[0], logs
}

func TestReferenceOutputArgumentsRejectedBeforeServerStart(t *testing.T) {
	service, path, _ := fixtureService(t, "ok")
	inspect := NewTools(service)[0]
	for _, invalid := range []map[string]any{{"output_mode": "unknown"}, {"include_declaration": "false"}} {
		invalid["operation"], invalid["file_path"], invalid["line"], invalid["column"] = "findReferences", path, 1, 1
		if _, err := inspect.Execute(t.Context(), invalid); err == nil {
			t.Fatalf("accepted invalid arguments: %v", invalid)
		}
	}
	if len(service.Manager().Activities()) != 0 {
		t.Fatal("started a server for invalid output arguments")
	}
}

func TestUnsupportedCapabilityDoesNotIssueRPC(t *testing.T) {
	service, path, logs := fixtureService(t, "good")
	for _, operation := range []string{"findReferences", "goToTypeDefinition", "previewRename"} {
		result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": operation, "file_path": path, "line": 1, "column": 1, "new_name": "renamed"})
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "code_graph") {
			t.Fatalf("%s: %s %v", operation, result.Content, err)
		}
	}
	log, _ := os.ReadFile(logs[0])
	for _, method := range []string{"textDocument/references", "textDocument/typeDefinition", "textDocument/rename"} {
		if strings.Contains(string(log), method) {
			t.Fatalf("unsupported RPC was sent: %s", method)
		}
	}
}

func TestUnsavedEditorBufferPreventsDiskPositionQuery(t *testing.T) {
	service, path, logs := fixtureService(t, "good")
	inspect := NewTools(service)[0]
	args := map[string]any{"operation": "hover", "file_path": path, "line": 1, "column": 1}
	if _, err := inspect.Execute(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	session, ok := service.Manager().ActiveSession(path)
	if !ok {
		t.Fatal("missing active session")
	}
	if _, err := session.SyncDocument(t.Context(), path, "unsaved value\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect.Execute(t.Context(), args); err == nil || !strings.Contains(err.Error(), "unsaved editor changes") {
		t.Fatalf("queried a different buffer: %v", err)
	}
	log, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "textDocument/hover") != 1 {
		t.Fatal("queried LSP with disk positions after buffer diverged")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "value\n" {
		t.Fatalf("disk changed: %q %v", data, err)
	}
	if _, err := session.SaveDocument(t.Context(), path, "value\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect.Execute(t.Context(), args); err != nil {
		t.Fatalf("saved matching content did not recover: %v", err)
	}
}

func TestWorkspaceSymbolFailuresAreNotEmptySuccess(t *testing.T) {
	for _, modes := range [][]string{{"fail"}, {"good", "fail"}} {
		t.Run(strings.Join(modes, "_"), func(t *testing.T) {
			service, _, _ := fixtureService(t, modes...)
			result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "workspaceSymbol", "query": "Found"})
			if len(modes) == 1 {
				if err == nil || !strings.Contains(err.Error(), "fixture search failed") {
					t.Fatalf("%s %v", result.Content, err)
				}
			} else if err != nil || !strings.Contains(result.Content, "Partial workspace results") || !strings.Contains(result.Content, "FoundSymbol") {
				t.Fatalf("%s %v", result.Content, err)
			}
			if strings.Contains(result.Content, "No symbols found") {
				t.Fatal("server failure reported as empty success")
			}
		})
	}
}

func TestCodeActionPreviewNeverExecutesCommands(t *testing.T) {
	service, path, logs := fixtureService(t, "good")
	result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "codeActions", "file_path": path, "action_title": "Run command"})
	if err == nil || !strings.Contains(err.Error(), "requires command") {
		t.Fatalf("%s %v", result.Content, err)
	}
	log, _ := os.ReadFile(logs[0])
	if strings.Contains(string(log), "workspace/executeCommand") {
		t.Fatal("read-only preview executed a command")
	}
}

func TestMissingDiagnosticsAreUnknown(t *testing.T) {
	service, path, _ := fixtureService(t, "good")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := NewTools(service)[0].Execute(ctx, map[string]any{"operation": "diagnostics", "file_path": path})
	if err != nil || !strings.Contains(result.Content, "No diagnostics data") || strings.Contains(result.Content, "No diagnostics found") {
		t.Fatalf("%s %v", result.Content, err)
	}
}

func TestStatusDoesNotStartDetectedServer(t *testing.T) {
	service, path, logs := fixtureService(t, "good")
	result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "status", "file_path": path})
	if err != nil || !strings.Contains(result.Content, "not running") {
		t.Fatalf("%s %v", result.Content, err)
	}
	if _, err := os.Stat(logs[0]); !os.IsNotExist(err) {
		t.Fatal("status started a server")
	}
}
