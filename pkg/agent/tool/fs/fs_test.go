package fs_test

import (
	"os"
	"testing"
)

func createTestRoot(t *testing.T) (*os.Root, string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "fs_test_*")

	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	root, err := os.OpenRoot(tmpDir)

	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to open root: %v", err)
	}

	cleanup := func() {
		root.Close()
		os.RemoveAll(tmpDir)
	}

	return root, tmpDir, cleanup
}
