package lsp_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
)

func TestNewToolsExposesSingleLSPTool(t *testing.T) {
	tools := NewTools(newTestService(t))
	if len(tools) != 1 || tools[0].Name != "lsp" {
		t.Fatalf("tools = %#v, want one lsp tool", tools)
	}
	lspTool := tools[0]

	props := lspTool.Parameters["properties"].(map[string]any)
	if got, want := slices.Sorted(maps.Keys(props)), []string{"character", "filePath", "line", "operation", "query", "symbol"}; !slices.Equal(got, want) {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
	enums := props["operation"].(map[string]any)["enum"].([]string)
	want := []string{
		"goToDefinition", "findReferences", "hover", "documentSymbol", "workspaceSymbol",
		"goToImplementation", "prepareCallHierarchy", "incomingCalls", "outgoingCalls",
	}
	if !slices.Equal(enums, want) {
		t.Fatalf("operations = %v, want %v", enums, want)
	}
	for _, operation := range want {
		if !strings.Contains(lspTool.Description, "- "+operation+":") {
			t.Errorf("description does not explain %s", operation)
		}
	}
	for _, key := range []string{"line", "character"} {
		if minimum, ok := props[key].(map[string]any)["minimum"].(int); !ok || minimum != 1 {
			t.Fatalf("%s minimum = %#v, want 1", key, props[key].(map[string]any)["minimum"])
		}
	}
	if required := lspTool.Parameters["required"].([]string); !slices.Equal(required, []string{"operation"}) {
		t.Fatalf("required = %#v", required)
	}
	if additional, ok := lspTool.Parameters["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("additionalProperties = %#v, want false", lspTool.Parameters["additionalProperties"])
	}
}

func TestLSPToolValidatesArgumentsBeforeAnyServer(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	for path, content := range map[string]string{filepath.Join(root, "main.go"): "package main\n", outside: "package outside\n"} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	service := newLanguageService(t, root)
	lspTool := NewTools(service)[0]

	for name, test := range map[string]struct {
		args map[string]any
		want string
	}{
		"zero line":           {map[string]any{"operation": "hover", "filePath": "main.go", "line": 0, "character": 1}, "line must be a positive 1-based integer"},
		"zero character":      {map[string]any{"operation": "hover", "filePath": "main.go", "line": 1, "character": 0}, "character must be a positive 1-based integer"},
		"fractional line":     {map[string]any{"operation": "hover", "filePath": "main.go", "line": 1.5, "character": 1}, "line must be a positive 1-based integer"},
		"no position":         {map[string]any{"operation": "hover", "filePath": "main.go"}, "position required"},
		"line only":           {map[string]any{"operation": "hover", "filePath": "main.go", "line": 1}, "character must be a positive 1-based integer"},
		"character only":      {map[string]any{"operation": "hover", "filePath": "main.go", "character": 3}, "line must be a positive 1-based integer"},
		"line out of range":   {map[string]any{"operation": "hover", "filePath": "main.go", "line": 99, "character": 1}, "out of range"},
		"symbol not on line":  {map[string]any{"operation": "hover", "filePath": "main.go", "line": 1, "symbol": "missing"}, `symbol "missing" not found on line 1`},
		"unknown operation":   {map[string]any{"operation": "diagnostics", "filePath": "main.go"}, "unknown LSP operation"},
		"missing file path":   {map[string]any{"operation": "documentSymbol"}, "filePath is required"},
		"missing file":        {map[string]any{"operation": "documentSymbol", "filePath": "absent.go"}, "file not found"},
		"directory":           {map[string]any{"operation": "documentSymbol", "filePath": "."}, "path is a directory"},
		"outside workspace":   {map[string]any{"operation": "documentSymbol", "filePath": outside}, "outside workspace"},
		"relative outside":    {map[string]any{"operation": "documentSymbol", "filePath": "../outside.go"}, "outside workspace"},
		"old parameter names": {map[string]any{"operation": "documentSymbol", "file_path": "main.go"}, "filePath is required"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lspTool.Execute(context.Background(), test.args); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if service.Running() {
		t.Fatal("invalid arguments started a language server")
	}
}

func newTestService(t *testing.T) *language.Service {
	t.Helper()
	return newLanguageService(t, t.TempDir())
}

func newLanguageService(t *testing.T, root string) *language.Service {
	t.Helper()
	service := language.New(root)
	t.Cleanup(service.Close)
	return service
}
