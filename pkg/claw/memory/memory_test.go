package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAgentRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents")

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sentinel := filepath.Join(root, "keep")
	if err := os.MkdirAll(sentinel, 0755); err != nil {
		t.Fatalf("mkdir sentinel: %v", err)
	}

	for _, name := range []string{"..", "../..", "../keep", "a/b", "global", ""} {
		if err := s.RemoveAgent(name); err == nil {
			t.Fatalf("RemoveAgent(%q) = nil, want error", name)
		}
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel dir was removed by traversal: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("agents dir was removed by traversal: %v", err)
	}
}

func TestRemoveAgentRemovesValidAgent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.EnsureAgent("worker"); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if _, err := os.Stat(s.AgentDir("worker")); err != nil {
		t.Fatalf("agent dir not created: %v", err)
	}
	if err := s.RemoveAgent("worker"); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if _, err := os.Stat(s.AgentDir("worker")); !os.IsNotExist(err) {
		t.Fatalf("agent dir not removed: %v", err)
	}
}
