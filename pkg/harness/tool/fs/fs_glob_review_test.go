package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func globReviewRoot(t *testing.T, files ...string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	for _, path := range files {
		if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(path, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGlobCancelledSearchWithoutMatches(t *testing.T) {
	root := globReviewRoot(t, "nested/file.txt")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := GlobTool(root).Execute(ctx, map[string]any{"pattern": "**/*.missing"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

type cancellingGlobContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancellingGlobContext) Err() error {
	c.checks++
	if c.checks == 5 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestGlobCancelsDuringTraversalWithoutMatches(t *testing.T) {
	root := globReviewRoot(t, "one/two/three/file.txt", "four/five/file.txt")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := GlobTool(root).Execute(&cancellingGlobContext{Context: ctx, cancel: cancel}, map[string]any{"pattern": "**/*.missing"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

func TestGlobDoesNotHideUnreadableDirectories(t *testing.T) {
	root := globReviewRoot(t, "restricted/file.txt", "visible.txt")
	if err := root.Chmod("restricted", 0); err != nil {
		t.Fatal(err)
	}
	defer root.Chmod("restricted", 0755)
	if _, err := os.ReadDir(filepath.Join(root.Name(), "restricted")); err == nil {
		t.Skip("directory permissions are not enforced")
	}
	_, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": "**/*.txt"})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want permission error", err)
	}
}

func TestGlobExplicitRelativeDepthAndLiteralSpaces(t *testing.T) {
	root := globReviewRoot(t, "file.txt", "nested/file.txt", " report.txt")
	for _, tc := range []struct{ pattern, want string }{
		{"./file.txt", "file.txt"},
		{"././file.txt", "file.txt"},
		{" report.txt", " report.txt"},
	} {
		result, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": tc.pattern})
		if err != nil || result.Content != tc.want {
			t.Fatalf("%q: %q, %v; want %q", tc.pattern, result.Content, err, tc.want)
		}
	}
}

func TestGlobPathIsLiteral(t *testing.T) {
	for _, directory := range []string{"[draft]", "{one,two}", "space dir"} {
		t.Run(directory, func(t *testing.T) {
			want := filepath.Join(directory, "file.txt")
			root := globReviewRoot(t, want, "other/file.txt")
			result, err := GlobTool(root).Execute(t.Context(), map[string]any{"path": directory, "pattern": "*.txt"})
			if err != nil || result.Content != want {
				t.Fatalf("result = %q, %v; want %q", result.Content, err, want)
			}
		})
	}
}

func TestGlobAbsolutePatternsPreserveDepth(t *testing.T) {
	root := globReviewRoot(t, "src/file.txt", "src/nested/file.txt")
	for _, pattern := range []string{"*.txt", "file.txt"} {
		result, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": filepath.Join(root.Name(), "src", pattern)})
		if err != nil || result.Content != filepath.Join("src", "file.txt") {
			t.Fatalf("%s: %q, %v", pattern, result.Content, err)
		}
	}

	missing, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": filepath.Join(root.Name(), "missing", "*.txt")})
	if err != nil || missing.Content != "No files found" {
		t.Fatalf("missing literal directory in absolute pattern: %q, %v", missing.Content, err)
	}
}

func TestGlobAbsoluteAndHomePatternsWithMetacharacterRoot(t *testing.T) {
	base := t.TempDir()
	wsDir := filepath.Join(base, "[workspace]")
	if err := os.MkdirAll(filepath.Join(wsDir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "src", "file.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	result, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": filepath.Join(wsDir, "src", "*.txt")})
	if err != nil || result.Content != filepath.Join("src", "file.txt") {
		t.Fatalf("metacharacter workspace root: %q, %v", result.Content, err)
	}

	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	homeResult, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": "~/[workspace]/src/*.txt"})
	if err != nil || homeResult.Content != filepath.Join("src", "file.txt") {
		t.Fatalf("home-relative pattern: %q, %v", homeResult.Content, err)
	}
}

func TestGlobLiteralPrefixesCannotBypassVCSFilter(t *testing.T) {
	root := globReviewRoot(t, ".git/objects/blob.txt", "src/.svn/entries.txt", "visible.txt")
	for _, pattern := range []string{".git/objects/*", ".git/objects/blob.txt", "src/.svn/**/*.txt", filepath.Join(root.Name(), ".git", "objects", "*.txt")} {
		result, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": pattern})
		if err != nil || result.Content != "No files found" {
			t.Fatalf("%s: %q, %v", pattern, result.Content, err)
		}
	}
}

func TestGlobExcludesMatchedSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks may require elevated privileges")
	}
	root := globReviewRoot(t, "real/file.txt")
	for name, target := range map[string]string{"link.txt": "real/file.txt", "linked": "real", "dangling.txt": "missing"} {
		if err := root.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, pattern := range []string{"**/*", "*.txt", "link.txt", "linked/file.txt", "linked/*.txt"} {
		result, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": pattern})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.Content, "link") {
			t.Fatalf("%s returned links: %s", pattern, result.Content)
		}
	}
	result, err := GlobTool(root).Execute(t.Context(), map[string]any{"path": "linked", "pattern": "*.txt"})
	if err != nil || result.Content != filepath.Join("linked", "file.txt") {
		t.Fatalf("explicit linked path: %q, %v", result.Content, err)
	}
}

func TestGlobStableLimitAndUniqueResults(t *testing.T) {
	root := globReviewRoot(t)
	stamp := time.Now().Add(-time.Hour)
	for i := range DefaultGlobLimit + 10 {
		name := fmt.Sprintf("f%03d.txt", i)
		if err := root.WriteFile(name, nil, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root.Name(), name), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	result, err := GlobTool(root).Execute(t.Context(), map[string]any{"pattern": "{*.txt,**/*.txt}"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) != DefaultGlobLimit+1 {
		t.Fatalf("got %d output lines", len(lines))
	}
	for i := range DefaultGlobLimit {
		if want := fmt.Sprintf("f%03d.txt", i); lines[i] != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[DefaultGlobLimit], "truncated") {
		t.Fatal("missing truncation notice")
	}
}
