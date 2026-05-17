package fs

import (
	"testing"
)

func TestTools(t *testing.T) {
	root, _, cleanup := createTestRoot(t)
	defer cleanup()

	tools := Tools(root)

	expectedNames := []string{"read", "write", "edit", "grep", "glob"}

	if len(tools) != len(expectedNames) {
		t.Errorf("expected %d tools, got %d", len(expectedNames), len(tools))
	}

	names := make(map[string]bool)

	for _, tool := range tools {
		names[tool.Name] = true

		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}

		if tool.Execute == nil {
			t.Errorf("tool %s has nil Execute function", tool.Name)
		}
	}

	for _, name := range expectedNames {
		if !names[name] {
			t.Errorf("missing expected tool: %s", name)
		}
	}
}
