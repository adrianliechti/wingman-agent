package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/coder/websocket/wsjson"
)

// useInstalledGopls lets a test start the gopls found on PATH.
func useInstalledGopls(t *testing.T) bool {
	t.Helper()
	path, err := exec.LookPath("gopls")
	if err != nil {
		return false
	}
	previous := lsp.CommandLookup
	lsp.CommandLookup = func(command string) string {
		if command == "gopls" {
			return path
		}
		return ""
	}
	t.Cleanup(func() { lsp.CommandLookup = previous })
	return true
}

func TestExternalGoIntelliSense(t *testing.T) {
	if !useInstalledGopls(t) {
		t.Skip("gopls is not installed")
	}
	source := "package main\n\nimport \"context\"\n\nfunc run(ctx context.Context) {\n\tctx.\n}\n"
	app, root := newLSPTestApp(t, map[string]string{"go.mod": "module tmp\n\ngo 1.24\n", "main.go": source})
	c := newLSPTestClient(t, app, root)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go"), "languageId": "go", "version": 1, "text": source}})

	params := c.position("main.go", 5, 5)
	params["context"] = map[string]any{"triggerKind": 2, "triggerCharacter": "."}
	var result struct {
		Items []struct {
			Label string `json:"label"`
		} `json:"items"`
	}
	c.result("textDocument/completion", params, &result)
	labels := map[string]bool{}
	for _, item := range result.Items {
		labels[item.Label] = true
	}
	for _, want := range []string{"Deadline", "Done", "Err", "Value"} {
		if !labels[want] {
			t.Fatalf("completion labels = %v, missing %q", labels, want)
		}
	}

	signature := "package main\n\nfunc consume(name string, count int) {}\n\nfunc run() {\n\tconsume(\n}\n"
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go"), "version": 2}, "contentChanges": []any{map[string]any{"text": signature}}})
	params = c.position("main.go", 5, 9)
	params["context"] = map[string]any{"triggerKind": 2, "triggerCharacter": "(", "isRetrigger": false}
	var help struct {
		Signatures []struct {
			Label      string `json:"label"`
			Parameters []any  `json:"parameters"`
		} `json:"signatures"`
	}
	c.result("textDocument/signatureHelp", params, &help)
	if len(help.Signatures) == 0 || !strings.Contains(help.Signatures[0].Label, "consume(name string, count int)") || len(help.Signatures[0].Parameters) != 2 {
		t.Fatalf("signatures = %+v, want consume with two parameters", help.Signatures)
	}
}

func TestExternalCommandEditsWaitForReview(t *testing.T) {
	if !useInstalledGopls(t) {
		t.Skip("gopls is not installed")
	}
	source := "package main\n\nfunc main() {}\n"
	app, root := newLSPTestApp(t, map[string]string{"go.mod": "module tmp\n\ngo 1.24\n", "main.go": source})
	c := newLSPTestClient(t, app, root)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go"), "languageId": "go", "version": 1, "text": source}})
	for _, accept := range []bool{false, true} {
		c.id++
		id := c.id
		if err := wsjson.Write(c.ctx, c.conn, map[string]any{"jsonrpc": "2.0", "id": id, "method": "workspace/executeCommand", "params": map[string]any{
			"textDocument": map[string]any{"uri": c.uri("main.go")},
			"command":      "gopls.add_import",
			"arguments":    []any{map[string]any{"ImportPath": "fmt", "URI": c.uri("main.go")}},
		}}); err != nil {
			t.Fatal(err)
		}
		reviewed := false
		for {
			var message map[string]any
			if err := wsjson.Read(c.ctx, c.conn, &message); err != nil {
				t.Fatal(err)
			}
			if message["method"] == "workspace/applyEdit" {
				reviewed = true
				if err := wsjson.Write(c.ctx, c.conn, map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{"applied": accept}}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if message["id"] == id {
				if !reviewed || message["error"] != nil && accept {
					t.Fatalf("accept=%v: reviewed=%v, response %v", accept, reviewed, message)
				}
				break
			}
		}
	}
}

func TestExternalWatchedFilesReachTheLanguageServer(t *testing.T) {
	if !useInstalledGopls(t) {
		t.Skip("gopls is not installed")
	}
	main := "package main\n\nfunc main() { helper() }\n"
	app, root := newLSPTestApp(t, map[string]string{"go.mod": "module tmp\n\ngo 1.24\n", "main.go": main})
	c := newLSPTestClient(t, app, root)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go"), "languageId": "go", "version": 1, "text": main}})
	waitForHelper := func(undefined bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			response := c.call("textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.go")}})
			var report struct {
				Kind  string                     `json:"kind"`
				Items []struct{ Message string } `json:"items"`
			}
			data, _ := json.Marshal(response["result"])
			if response["error"] == nil && json.Unmarshal(data, &report) == nil && report.Kind == "full" {
				found := false
				for _, item := range report.Items {
					found = found || strings.Contains(item.Message, "undefined: helper")
				}
				if found == undefined {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for undefined helper diagnostic = %v", undefined)
	}
	waitForHelper(true)
	path := filepath.Join(root, "helper.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc helper() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForHelper(false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitForHelper(true)
}

func TestExternalDefinitionNavigation(t *testing.T) {
	if !useInstalledGopls(t) {
		t.Skip("gopls is not installed")
	}
	main := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	app, root := newLSPTestApp(t, map[string]string{"go.mod": "module tmp\n\ngo 1.24\n", "main.go": main})
	c := newLSPTestClient(t, app, root)

	var locations []lsp.Location
	for deadline := time.Now().Add(60 * time.Second); len(locations) == 0 && time.Now().Before(deadline); {
		if response := c.call("textDocument/definition", c.position("main.go", 5, 5)); response["error"] == nil {
			c.result("textDocument/definition", c.position("main.go", 5, 5), &locations)
		}
		if len(locations) == 0 {
			time.Sleep(time.Second)
		}
	}
	if len(locations) == 0 {
		t.Fatal("no definition locations for fmt.Println")
	}
	path, virtual, ok := app.lspSourceTarget(string(locations[0].URI))
	if !ok || virtual != "" || !strings.HasPrefix(string(locations[0].URI), lspSourceScheme+":/") {
		t.Fatalf("expected an external wingman-source file, got %+v", locations[0])
	}
	if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
		t.Fatalf("expected external location, got %+v", locations[0])
	}

	var content struct {
		Text string `json:"text"`
	}
	c.result("workspace/textDocumentContent", map[string]any{"uri": string(locations[0].URI)}, &content)
	if !strings.Contains(content.Text, "func Println(") {
		t.Fatalf("external file %s does not contain the Println definition", path)
	}
	for _, uri := range []string{fileuri.FromPath("/etc/hosts"), lspSourceScheme + ":///etc/hosts"} {
		if response := c.call("workspace/textDocumentContent", map[string]any{"uri": uri}); response["error"] == nil {
			t.Fatalf("unreported external file %s was exposed", uri)
		}
	}
}
