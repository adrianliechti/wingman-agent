package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestHistoryProjectDirectoryLinks(t *testing.T) {
	testenv.UserHome(t)
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			target := t.TempDir()
			root := filepath.Join(t.TempDir(), "workspace")
			testenv.DirLink(t, kind, target, root)
			want := projectDirsFor(target, nil)
			if got := projectDirsFor(root, nil); len(want) != 1 || !slices.Equal(got, want) {
				t.Fatalf("history project = %q, want %q", got, want)
			}
		})
	}
}

func TestLongHistoryRecordedCwdLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			config := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", config)
			target := filepath.Join(t.TempDir(), filepath.FromSlash(strings.Repeat("long-segment/", 20)))
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			resolved, err := pathutil.Resolve(target)
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "workspace")
			testenv.DirLink(t, kind, target, alias)
			key := encodeCwd(resolved)
			path := filepath.Join(config, "projects", key[:maxProjectKeyLen]+"-hash", "s.jsonl")
			writeHistoryTranscript(t, path, alias, "linked project")
			for _, cwd := range []string{target, alias} {
				if listed, err := listProjectSessions(cwd, nil); err != nil || len(listed) != 1 {
					t.Fatalf("list %s = %+v, %v", cwd, listed, err)
				}
				if got := transcriptPath(cwd, "s", nil); got != path {
					t.Fatalf("transcript for %s = %q, want %q", cwd, got, path)
				}
			}
		})
	}
}
