package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestWorkspaceIdentityAndDebugFramesThroughDirectoryLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			target := t.TempDir()
			outside := t.TempDir()
			for _, dir := range []string{target, outside} {
				if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Join(t.TempDir(), "workspace")
			testenv.DirLink(t, kind, target, root)
			testenv.DirLink(t, kind, outside, filepath.Join(target, "escape"))
			actual, alias := workspaceScope(target), workspaceScope(root)
			if actual.WorkspaceID != alias.WorkspaceID || actual.InstanceID == alias.InstanceID {
				t.Fatalf("workspace identity mismatch: actual = %+v, alias = %+v", actual, alias)
			}
			// Adapters report resolved paths; frames keep the workspace's own spelling.
			c := &dapConnection{server: &Server{workspace: &code.Workspace{RootPath: root}}}
			resolvedRoot, _ := pathutil.Resolve(root)
			for _, path := range []string{filepath.Join(target, "main.go"), filepath.Join(root, "main.go")} {
				if got := c.framePath(path, resolvedRoot); got != filepath.Join(root, "main.go") {
					t.Errorf("frame %q mapped to %q", path, got)
				}
			}
			if path := filepath.Join(outside, "main.go"); c.framePath(path, resolvedRoot) != path {
				t.Errorf("outside frame %q was mapped into the workspace", path)
			}
		})
	}
}
