package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
)

// Ported from pi's find regression #3302: directory-prefixed and
// path-segment patterns must match inside the subtree, not only basenames.
func TestGlobMatchesPathPrefixedPatterns(t *testing.T) {
	root, tmpDir, cleanup := createTestRoot(t)
	defer cleanup()
	for _, p := range []string{"some/parent/child/test.spec.ts", "src/foo/bar/example.spec.ts", "src/top.spec.ts", "other.spec.ts"} {
		full := filepath.Join(tmpDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	globTool := GlobTool(root)
	for _, tc := range []struct {
		pattern string
		want    []string
		absent  []string
	}{
		{"**/*.spec.ts", []string{"some/parent/child/test.spec.ts", "src/foo/bar/example.spec.ts", "src/top.spec.ts", "other.spec.ts"}, nil},
		{"some/parent/child/**", []string{"some/parent/child/test.spec.ts"}, []string{"src/", "other.spec.ts"}},
		{"**/parent/child/*", []string{"some/parent/child/test.spec.ts"}, []string{"src/", "other.spec.ts"}},
		{"src/**/*.spec.ts", []string{"src/foo/bar/example.spec.ts", "src/top.spec.ts"}, []string{"some/", "other.spec.ts"}},
	} {
		result, err := globTool.Execute(context.Background(), map[string]any{"pattern": tc.pattern})
		if err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		content := filepath.ToSlash(result.Content)
		for _, want := range tc.want {
			if !strings.Contains(content, want) {
				t.Errorf("%s: missing %s in:\n%s", tc.pattern, want, result.Content)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(content, absent) {
				t.Errorf("%s: unexpected %s in:\n%s", tc.pattern, absent, result.Content)
			}
		}
	}
}
