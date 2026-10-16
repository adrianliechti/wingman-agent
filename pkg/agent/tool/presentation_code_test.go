package tool

import (
	"strings"
	"testing"
)

func TestCodeInspectionPresentationPreservesLocationSemantics(t *testing.T) {
	for _, name := range []string{"lsp", "Inspect code"} {
		got := Present(name, "", `{"operation":"hover","file_path":"main.go","line":3,"column":2}`, false)
		if got.Path != "main.go" || got.Line != 3 || got.Kind != "read" {
			t.Fatalf("LSP location lost: %+v", got)
		}
	}
	for _, name := range []string{"code_graph", "Inspect code graph"} {
		got := Present(name, "", `{"operation":"search_content","pattern":"needle","file":"handler"}`, false)
		if got.Path != "" || got.Hint != "needle" || !strings.Contains(got.Args, `"file":"handler"`) {
			t.Fatalf("graph filter presented as file location: %+v", got)
		}
	}
}
