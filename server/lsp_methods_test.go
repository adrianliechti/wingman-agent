package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type lspTestClient struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
	id   float64
	root string
}

func newLSPTestApp(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app, err := New(context.Background(), root, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, root
}

func newLSPTestClient(t *testing.T, app *Server, root string) *lspTestClient {
	t.Helper()
	conn, ctx := dialEditorTest(t, app, "/lsp")
	conn.SetReadLimit(16 << 20) // external sources exceed the default 32 KiB
	c := &lspTestClient{t: t, conn: conn, ctx: ctx, root: root}
	c.result("initialize", map[string]any{"capabilities": map[string]any{}}, nil)
	c.notify("initialized", map[string]any{})
	return c
}

func (c *lspTestClient) uri(name string) string { return fileuri.FromPath(filepath.Join(c.root, name)) }

func (c *lspTestClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	return editorTestReply(c.t, c.conn, c.ctx, map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
}

func (c *lspTestClient) result(method string, params any, out any) {
	c.t.Helper()
	response := c.call(method, params)
	if response["error"] != nil {
		c.t.Fatalf("%s: %v", method, response["error"])
	}
	if out != nil {
		data, _ := json.Marshal(response["result"])
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s result %s: %v", method, data, err)
		}
	}
}

func (c *lspTestClient) notify(method string, params any) {
	c.t.Helper()
	if err := wsjson.Write(c.ctx, c.conn, map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *lspTestClient) position(name string, line, character int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": c.uri(name)}, "position": map[string]any{"line": line, "character": character}}
}

func TestEditorCapabilityOptions(t *testing.T) {
	t.Run("rename", func(t *testing.T) {
		prepare := true
		tests := []struct {
			name        string
			provider    any
			wantEnabled bool
			wantPrepare bool
		}{
			{name: "missing"},
			{name: "disabled", provider: lsp.Boolean(false)},
			{name: "boolean", provider: lsp.Boolean(true), wantEnabled: true},
			{name: "options", provider: &lsp.RenameOptions{}, wantEnabled: true},
			{name: "prepare", provider: &lsp.RenameOptions{PrepareProvider: &prepare}, wantEnabled: true, wantPrepare: true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				enabled, canPrepare := renameCapabilities(test.provider)
				if enabled != test.wantEnabled || canPrepare != test.wantPrepare {
					t.Fatalf("renameCapabilities() = (%v, %v), want (%v, %v)", enabled, canPrepare, test.wantEnabled, test.wantPrepare)
				}
			})
		}
	})

	t.Run("code action", func(t *testing.T) {
		resolve := true
		tests := []struct {
			name        string
			provider    any
			wantEnabled bool
			wantResolve bool
		}{
			{name: "missing"},
			{name: "disabled", provider: lsp.Boolean(false)},
			{name: "boolean", provider: lsp.Boolean(true), wantEnabled: true},
			{name: "options", provider: &lsp.CodeActionOptions{}, wantEnabled: true},
			{name: "resolve", provider: &lsp.CodeActionOptions{ResolveProvider: &resolve}, wantEnabled: true, wantResolve: true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				enabled, canResolve := codeActionCapabilities(test.provider)
				if enabled != test.wantEnabled || canResolve != test.wantResolve {
					t.Fatalf("codeActionCapabilities() = (%v, %v), want (%v, %v)", enabled, canResolve, test.wantEnabled, test.wantResolve)
				}
			})
		}
	})
}

func TestLSPStructuralFallbacks(t *testing.T) {
	source := "package main\n\nfunc main() {\n\tgreet()\n}\n\nfunc greet() {}\n"
	app, root := newLSPTestApp(t, map[string]string{"main.go": source})
	c := newLSPTestClient(t, app, root)

	t.Run("definition", func(t *testing.T) {
		var locations []lsp.Location
		c.result("textDocument/definition", c.position("main.go", 3, 1), &locations)
		if len(locations) != 1 {
			t.Fatalf("locations = %+v, want one", locations)
		}
		path, ok := fileuri.Path(string(locations[0].URI))
		// Language servers canonicalize drive letters (gopls reports
		// file:///f%3A/...), so compare file identity rather than spelling.
		if !ok || !sameFile(t, path, filepath.Join(root, "main.go")) || locations[0].Range.Start.Line != 6 || locations[0].Range.Start.Character != 5 {
			t.Fatalf("location = %+v, want main.go:7:6", locations[0])
		}
	})

	t.Run("references", func(t *testing.T) {
		params := c.position("main.go", 6, 5)
		params["context"] = map[string]any{"includeDeclaration": true}
		var locations []lsp.Location
		c.result("textDocument/references", params, &locations)
		if len(locations) != 2 || locations[0].Range.Start.Line != 3 || locations[1].Range.Start.Line != 6 {
			t.Fatalf("locations = %+v, want call site and declaration", locations)
		}
		params["context"] = map[string]any{"includeDeclaration": false}
		c.result("textDocument/references", params, &locations)
		if len(locations) != 1 || locations[0].Range.Start.Line != 3 {
			t.Fatalf("locations = %+v, want only the call site", locations)
		}
	})

	t.Run("document symbols", func(t *testing.T) {
		var symbols []struct {
			Name string `json:"name"`
			Kind int    `json:"kind"`
		}
		c.result("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}}, &symbols)
		if len(symbols) != 2 || symbols[0].Name != "main" || symbols[1].Name != "greet" || symbols[0].Kind != 12 {
			t.Fatalf("symbols = %+v, want functions main and greet", symbols)
		}
	})

	t.Run("tree-sitter completions", func(t *testing.T) {
		var result struct {
			Items []struct {
				Label  string `json:"label"`
				Kind   int    `json:"kind"`
				Detail string `json:"detail"`
			} `json:"items"`
		}
		c.result("textDocument/completion", c.position("main.go", 3, 1), &result)
		if len(result.Items) != 2 || result.Items[0].Label != "main" || result.Items[1].Label != "greet" {
			t.Fatalf("items = %+v, want main and greet", result.Items)
		}
		if !strings.Contains(result.Items[0].Detail, "tree-sitter") || result.Items[0].Kind != 3 {
			t.Fatalf("item = %+v, want a tree-sitter function completion", result.Items[0])
		}
		params := c.position("main.go", 3, 1)
		params["context"] = map[string]any{"triggerKind": 2, "triggerCharacter": "."}
		c.result("textDocument/completion", params, &result)
		if len(result.Items) != 0 {
			t.Fatalf("member fallback items = %+v, want none", result.Items)
		}
	})

	t.Run("hover", func(t *testing.T) {
		var hover struct {
			Contents struct {
				Value string `json:"value"`
			} `json:"contents"`
		}
		c.result("textDocument/hover", c.position("main.go", 3, 1), &hover)
		if !strings.Contains(hover.Contents.Value, "func greet()") || !strings.Contains(hover.Contents.Value, "main.go:7") {
			t.Fatalf("contents = %q, want snippet and location", hover.Contents.Value)
		}
	})

	t.Run("type-aware methods without a language server", func(t *testing.T) {
		var locations []lsp.Location
		c.result("textDocument/implementation", c.position("main.go", 6, 5), &locations)
		if len(locations) != 0 {
			t.Fatalf("implementations = %+v, want none", locations)
		}
		if response := c.call("textDocument/signatureHelp", c.position("main.go", 3, 1)); response["error"] != nil || response["result"] != nil {
			t.Fatalf("signature help = %v, want null", response)
		}
		var report struct {
			Kind  string `json:"kind"`
			Items []any  `json:"items"`
		}
		c.result("textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}}, &report)
		if report.Kind != "full" || report.Items == nil || len(report.Items) != 0 {
			t.Fatalf("diagnostics = %+v, want an empty full report", report)
		}
	})

	t.Run("unsaved buffers", func(t *testing.T) {
		// A new file's buffer can be opened before it is first written.
		for name, text := range map[string]string{"main.go": "package main\n\nfunc renamed() {}\n", "new.go": "package main\n\nfunc fresh() {}\n"} {
			c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": c.uri(name), "languageId": "go", "version": 1, "text": text}})
		}
		for name, want := range map[string]string{"main.go": "renamed", "new.go": "fresh"} {
			var symbols []struct {
				Name string `json:"name"`
			}
			c.result("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": c.uri(name)}}, &symbols)
			if len(symbols) != 1 || symbols[0].Name != want {
				t.Fatalf("%s symbols = %+v, want %s", name, symbols, want)
			}
		}
	})
}

func TestLSPRequestValidation(t *testing.T) {
	app, root := newLSPTestApp(t, map[string]string{"main.go": "package main\n"})
	c := newLSPTestClient(t, app, root)
	for name, params := range map[string]any{
		"malformed position": map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}, "position": "start"},
		"negative position":  c.position("main.go", -1, 0),
		"outside workspace":  map[string]any{"textDocument": map[string]any{"uri": fileuri.FromPath(filepath.Join(root, "..", "main.go"))}, "position": map[string]any{"line": 0, "character": 0}},
		"not a file URI":     map[string]any{"textDocument": map[string]any{"uri": "untitled:main.go"}, "position": map[string]any{"line": 0, "character": 0}},
	} {
		if response := c.call("textDocument/definition", params); response["error"] == nil {
			t.Fatalf("%s accepted: %v", name, response)
		}
	}
	if response := c.call("textDocument/unknown", map[string]any{}); response["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", response)
	}
}

func TestLSPWorkspaceDiagnosticsAndActivity(t *testing.T) {
	app, root := newLSPTestApp(t, nil)
	c := newLSPTestClient(t, app, root)
	var report struct {
		Items   []json.RawMessage `json:"items"`
		Wingman struct {
			CheckedFiles       *int     `json:"checked_files"`
			UnavailableServers []string `json:"unavailable_servers"`
		} `json:"_wingman"`
	}
	c.result("workspace/diagnostic", map[string]any{"previousResultIds": []any{}}, &report)
	if report.Items == nil || report.Wingman.UnavailableServers == nil || report.Wingman.CheckedFiles == nil || *report.Wingman.CheckedFiles != 0 {
		t.Fatalf("workspace report = %+v", report)
	}
	var activity lspActivityResponse
	c.result("wingman/activity", map[string]any{}, &activity)
	if activity.Analyzing || activity.Services == nil || len(activity.Services) != 0 {
		t.Fatalf("activity = %+v", activity)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(infoA, infoB)
}
