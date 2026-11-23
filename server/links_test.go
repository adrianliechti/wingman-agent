package server

import (
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestWorkspaceIdentityThroughDirectoryLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			target := t.TempDir()
			root := filepath.Join(t.TempDir(), "workspace")
			testenv.DirLink(t, kind, target, root)
			actual, alias := workspaceScope(target), workspaceScope(root)
			if actual.WorkspaceID != alias.WorkspaceID || actual.InstanceID == alias.InstanceID {
				t.Fatalf("workspace identity mismatch: actual = %+v, alias = %+v", actual, alias)
			}
		})
	}
}
