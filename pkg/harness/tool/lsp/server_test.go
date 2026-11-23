package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

// A real subprocess exercises initialization, capability detection and failed
// RPCs through the production session manager.
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
			Params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Item map[string]any `json:"item"`
			} `json:"params"`
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
		first := map[string]any{"start": map[string]int{"line": 0, "character": 0}, "end": map[string]int{"line": 0, "character": 5}}
		location := map[string]any{"uri": request.Params.TextDocument.URI, "range": first}
		item := map[string]any{"name": "value", "kind": 12, "uri": request.Params.TextDocument.URI, "range": first, "selectionRange": first}
		var result any
		var rpcError any
		fail := func(message string) { rpcError = map[string]any{"code": -32603, "message": message} }
		switch request.Method {
		case "initialize":
			capabilities := map[string]any{"textDocumentSync": 1, "hoverProvider": true}
			if mode != "minimal" {
				for _, provider := range []string{"workspaceSymbol", "documentSymbol", "definition", "references", "implementation", "callHierarchy"} {
					capabilities[provider+"Provider"] = true
				}
			}
			result = map[string]any{"capabilities": capabilities}
		case "textDocument/hover":
			switch mode {
			case "fail":
				fail("fixture server failed")
			case "slow":
				time.Sleep(5 * time.Second)
				fallthrough
			default:
				result = map[string]any{"contents": "fixture hover"}
			}
		case "textDocument/definition":
			result = location
		case "textDocument/references":
			result = []any{location, location}
		case "textDocument/implementation":
			result = []any{}
		case "textDocument/prepareCallHierarchy":
			result = []any{item}
		case "callHierarchy/incomingCalls":
			result = []any{map[string]any{"from": request.Params.Item, "fromRanges": []any{first}}}
		case "callHierarchy/outgoingCalls":
			result = []any{}
		case "workspace/symbol":
			switch mode {
			case "fail":
				fail("fixture search failed")
			case "uri-only":
				result = []any{map[string]any{"name": "DeferredSymbol", "kind": 12, "location": map[string]any{"uri": "file:///fixture/two.other"}, "data": 1}}
			default:
				result = []any{map[string]any{"name": "FoundSymbol", "kind": 12, "location": map[string]any{"uri": "file:///fixture/one.mock", "range": first}}}
			}
		case "textDocument/documentSymbol":
			if mode == "fail-symbols" {
				fail("fixture symbol lookup failed")
			} else {
				result = []any{map[string]any{"name": "value", "kind": 13, "range": first, "selectionRange": first}}
			}
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

func fixtureServer(t *testing.T, extension, mode string) (lsp.Server, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "rpc.log")
	return lsp.Server{
		Name:                extension,
		Command:             executable,
		Args:                []string{"-test.run=^TestLSPFixtureServer$", "--", "lsp-fixture", mode, log},
		ExtensionToLanguage: map[string]string{"." + extension: extension},
	}, log
}

// fixtureService serves .mock files with this test binary as language server.
func fixtureService(t *testing.T, mode string, configure ...func(*lsp.Server)) (*language.Service, string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "one.mock")
	if err := os.WriteFile(path, []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server, log := fixtureServer(t, "mock", mode)
	for _, change := range configure {
		change(&server)
	}
	service := language.New(root, server)
	t.Cleanup(service.Close)
	return service, path, log
}

func TestWorkspaceSymbolIncludesBothLanguageServers(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.mock", "two.other"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ranged, _ := fixtureServer(t, "mock", "good")
	deferred, _ := fixtureServer(t, "other", "uri-only")
	service := language.New(root, ranged, deferred)
	defer service.Close()
	result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "workspaceSymbol", "query": "Symbol"})
	if err != nil {
		t.Fatal(err)
	}
	output := result.Content.Text()
	for _, want := range []string{"Symbols (2 found):", "FoundSymbol (Function) - Line 1\n", "DeferredSymbol (Function)\n"} {
		if !strings.Contains(output, want) {
			t.Errorf("result = %q, missing %q", output, want)
		}
	}
}

func TestOperationsAnswerThroughTheLanguageServer(t *testing.T) {
	service, path, _ := fixtureService(t, "good")
	inspect := NewTools(service)[0]
	for operation, want := range map[string][]string{
		"hover":                {"fixture hover"},
		"goToDefinition":       {"Definition (1 found):", "one.mock:1:1", "     1\tvalue"},
		"findReferences":       {"References (2 found across 1 files):", "one.mock:1:1: value"},
		"goToImplementation":   {"No implementations found"},
		"prepareCallHierarchy": {"Call hierarchy items (1 found):", "value (Function) - one.mock:1"},
		"incomingCalls":        {"Incoming Calls (1 found):", "value (Function) - one.mock:1, call sites at lines 1"},
		"outgoingCalls":        {"No outgoing calls found"},
		"documentSymbol":       {"Symbols (1 found):", "one.mock:", "value (Variable) - Line 1"},
		"workspaceSymbol":      {"Symbols (1 found):", "FoundSymbol (Function) - Line 1"},
	} {
		t.Run(operation, func(t *testing.T) {
			result, err := inspect.Execute(t.Context(), map[string]any{"operation": operation, "filePath": path, "line": 1, "character": 1, "query": "Found"})
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range want {
				if !strings.Contains(result.Content.Text(), text) {
					t.Fatalf("result = %q, want %q", result.Content.Text(), text)
				}
			}
		})
	}
	result, err := inspect.Execute(t.Context(), map[string]any{"operation": "hover", "filePath": "one.mock", "symbol": "value"})
	if err != nil || result.Content.Text() != "fixture hover" {
		t.Fatalf("hover by symbol and relative path = %q, %v", result.Content.Text(), err)
	}
	result, err = inspect.Execute(t.Context(), map[string]any{"operation": "workspaceSymbol", "query": "Found"})
	if err != nil || !strings.Contains(result.Content.Text(), "FoundSymbol (Function)") {
		t.Fatalf("workspace symbols without a file = %q, %v", result.Content.Text(), err)
	}
}

func TestServerStartsOnlyForAValidQuery(t *testing.T) {
	service, path, log := fixtureService(t, "good")
	inspect := NewTools(service)[0]
	for _, args := range []map[string]any{
		{"operation": "hover"},
		{"operation": "hover", "filePath": true},
		{"operation": "workspaceSymbol", "filePath": path, "query": 2},
		{"operation": "status", "filePath": path},
		{"operation": "hover", "filePath": path},
		{"operation": "hover", "filePath": path, "symbol": "value", "character": 1},
		{"operation": "hover", "filePath": path, "symbol": "value", "line": nil},
		{"operation": "hover", "filePath": path, "line": 99, "character": 1},
		{"operation": "hover", "filePath": path, "line": 1, "symbol": "absent"},
	} {
		if _, err := inspect.Execute(t.Context(), args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) || service.Running() {
		t.Fatal("invalid arguments started the language server")
	}
	if _, err := inspect.Execute(t.Context(), map[string]any{"operation": "hover", "filePath": path, "line": 1, "character": 1}); err != nil {
		t.Fatal(err)
	}
	if !service.Running() {
		t.Fatal("a valid query did not start the language server")
	}
	if len(NewTools(nil)) != 0 {
		t.Fatal("nil service exposed a tool")
	}
}

func TestUnsupportedCapabilityDoesNotIssueRPC(t *testing.T) {
	service, path, log := fixtureService(t, "minimal")
	for _, operation := range []string{"findReferences", "goToDefinition", "incomingCalls", "documentSymbol"} {
		result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": operation, "filePath": path, "line": 1, "character": 1})
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "grep") {
			t.Fatalf("%s: %s %v", operation, result.Content.Text(), err)
		}
	}
	data, _ := os.ReadFile(log)
	for _, method := range []string{"textDocument/references", "textDocument/definition", "textDocument/prepareCallHierarchy", "textDocument/documentSymbol"} {
		if strings.Contains(string(data), method) {
			t.Fatalf("unsupported RPC was sent: %s", method)
		}
	}
}

func TestUnsavedEditorBufferPreventsDiskPositionQuery(t *testing.T) {
	service, path, log := fixtureService(t, "good")
	inspect := NewTools(service)[0]
	args := map[string]any{"operation": "hover", "filePath": path, "line": 1, "character": 1}
	if _, err := inspect.Execute(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	session, ok := service.Manager().ActiveSession(path)
	if !ok {
		t.Fatal("missing active session")
	}
	if _, err := session.SyncEditorDocument(t.Context(), path, "unsaved value\n", false); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect.Execute(t.Context(), args); err == nil || !strings.Contains(err.Error(), "differs from disk") {
		t.Fatalf("queried a different buffer: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "textDocument/hover") != 1 {
		t.Fatal("queried LSP with disk positions after buffer diverged")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "value\n" {
		t.Fatalf("disk changed: %q %v", data, err)
	}
	if _, err := session.SyncEditorDocument(t.Context(), path, "value\n", true); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect.Execute(t.Context(), args); err != nil {
		t.Fatalf("saved matching content did not recover: %v", err)
	}
}

func TestServerFailuresAreErrorsNotEmptyResults(t *testing.T) {
	service, path, _ := fixtureService(t, "fail")
	inspect := NewTools(service)[0]
	for operation, want := range map[string]string{"hover": "fixture server failed", "workspaceSymbol": "fixture search failed"} {
		result, err := inspect.Execute(t.Context(), map[string]any{"operation": operation, "filePath": path, "line": 1, "character": 1, "query": "Found"})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "grep") {
			t.Fatalf("%s: %q %v", operation, result.Content.Text(), err)
		}
	}

	service, path, _ = fixtureService(t, "fail-symbols")
	result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "hover", "filePath": path, "symbol": "value"})
	if err == nil || !strings.Contains(err.Error(), "fixture symbol lookup failed") {
		t.Fatalf("symbol lookup failure: %q %v", result.Content.Text(), err)
	}
}

func TestMissingServerIsReportedWithTheFallback(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"notes.txt": "value\n", "one.mock": "value\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(root, "missing-ls")
	service := language.New(root, lsp.Server{Name: "absent", Command: missing, ExtensionToLanguage: map[string]string{".mock": "mock"}})
	defer service.Close()
	inspect := NewTools(service)[0]
	if _, err := inspect.Execute(t.Context(), map[string]any{"operation": "workspaceSymbol", "query": "x"}); err == nil || !strings.Contains(err.Error(), "no file in the workspace has an installed language server") {
		t.Fatalf("workspace symbols without a server = %v", err)
	}
	for file, want := range map[string]string{
		"notes.txt": "no language server is configured for .txt files",
		"one.mock":  "language server absent for .mock files is not installed: \"" + missing + "\" not found on PATH",
	} {
		result, err := inspect.Execute(t.Context(), map[string]any{"operation": "hover", "filePath": file, "line": 1, "character": 1})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "grep") {
			t.Fatalf("%s: %q %v", file, result.Content.Text(), err)
		}
	}
	if service.Running() {
		t.Fatal("a missing server was reported running")
	}
}

func TestSlowServerEndsAtItsRequestTimeout(t *testing.T) {
	service, path, _ := fixtureService(t, "slow", func(server *lsp.Server) { server.RequestTimeout, server.ShutdownTimeout = 200, 100 })
	started := time.Now()
	result, err := NewTools(service)[0].Execute(t.Context(), map[string]any{"operation": "hover", "filePath": path, "line": 1, "character": 1})
	if err == nil || !strings.Contains(err.Error(), "did not answer within 200ms") || time.Since(started) > 3*time.Second {
		t.Fatalf("slow query = %q, %v after %s", result.Content.Text(), err, time.Since(started))
	}
}

func TestCancelledRequestsReturnTheContextError(t *testing.T) {
	service, path, _ := fixtureService(t, "good")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := NewTools(service)[0].Execute(ctx, map[string]any{"operation": "hover", "filePath": path, "line": 1, "character": 1})
	if !errors.Is(err, context.Canceled) || result.Content.Text() != "" {
		t.Fatalf("cancelled query = %q, %v", result.Content.Text(), err)
	}
}
