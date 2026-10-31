package tool

import "testing"

func TestCodeInspectionPresentationPreservesLocationSemantics(t *testing.T) {
	for _, name := range []string{"lsp", "Inspect code"} {
		got := Present(name, "", `{"operation":"hover","file_path":"main.go","line":3,"column":2}`, false)
		if len(got.Locations) != 1 || got.Locations[0] != (Location{Path: "main.go", Line: 3}) || got.Kind != "read" {
			t.Fatalf("LSP location lost: %+v", got)
		}
	}
}
