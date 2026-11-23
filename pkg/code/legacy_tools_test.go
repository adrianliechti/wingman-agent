package code

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestRemoveLegacyToolsDeletesOnlyTheToolsDirectory(t *testing.T) {
	home := testenv.WingmanHome(t)
	if err := RemoveLegacyTools(); err != nil {
		t.Fatalf("without a tools directory: %v", err)
	}
	for _, name := range []string{"tools/gopls/bin/gopls", "tools/.lock", "sessions/keep.json"} {
		path := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveLegacyTools(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "tools")); !os.IsNotExist(err) {
		t.Fatalf("tools directory remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", "keep.json")); err != nil {
		t.Fatalf("other Wingman data was removed: %v", err)
	}
}
