package acp

import (
	"path/filepath"
	"runtime"
	"testing"
)

// testRoot is an absolute root on the current platform: /workspace on Unix
// and C:\workspace on Windows.
func testRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\workspace`
	}
	return "/workspace"
}

func TestNormalizeSessionRoots(t *testing.T) {
	root := testRoot()
	cwd, additional, err := NormalizeSessionRoots(root+"/./app", []string{
		root + "/app", root + "/lib/../lib", root + "/lib",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cwd != filepath.Join(root, "app") || len(additional) != 1 || additional[0] != filepath.Join(root, "lib") {
		t.Fatalf("roots = %q, %q", cwd, additional)
	}

	for _, tc := range []struct {
		cwd        string
		additional []string
	}{
		{cwd: "relative"},
		{cwd: root + "/\x00bad"},
		{cwd: root, additional: []string{"relative"}},
		{cwd: root, additional: []string{""}},
		{cwd: root, additional: []string{root + "/\x00bad"}},
	} {
		if _, _, err := NormalizeSessionRoots(tc.cwd, tc.additional); err == nil {
			t.Fatalf("expected invalid roots to fail: %#v", tc)
		}
	}
}

func TestResolvePath(t *testing.T) {
	root := testRoot()
	for _, tc := range []struct{ path, cwd, want string }{
		{path: "", cwd: root, want: ""},
		{path: "pkg/main.go", cwd: "", want: "pkg/main.go"},
		{path: "pkg/main.go", cwd: root, want: filepath.Join(root, "pkg", "main.go")},
		{path: filepath.Join(root, "main.go"), cwd: root, want: filepath.Join(root, "main.go")},
		// A rooted path is never nested beneath cwd; on Windows it takes cwd's volume.
		{path: "/other/main.go", cwd: root, want: filepath.VolumeName(root) + "/other/main.go"},
	} {
		if got := ResolvePath(tc.path, tc.cwd); got != tc.want {
			t.Errorf("ResolvePath(%q, %q) = %q, want %q", tc.path, tc.cwd, got, tc.want)
		}
	}
}
