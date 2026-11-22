package fs

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMatchAllowedRootHandlesCaseFoldingLengthChanges(t *testing.T) {
	volume := filepath.VolumeName(t.TempDir())
	foldCase := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	cases := []struct {
		name    string
		root    string
		path    string
		wantSub string
		wantOK  bool
	}{
		{"ascii nested", "/tmp/ok", "/tmp/ok/sub/file.txt", "sub/file.txt", true},
		{"exact root", "/tmp/ok", "/tmp/ok", "", true},
		{"volume root", "/", "/tmp/ok/file.txt", "tmp/ok/file.txt", true},
		{"sibling prefix is not a match", "/tmp/ok", "/tmp/okay/file.txt", "", false},
		{"lowercase shrinks encoded length", "/tmp/ẞẞ", "/tmp/ßß/x", "x", foldCase},
		{"lowercase grows encoded length", "/tmp/İ", "/tmp/i/x", "x", foldCase},
		{"case-insensitive ascii", "/tmp/Work", "/tmp/work/x", "x", foldCase},
		{"unrelated root", "/tmp/ok", "/other/file.txt", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := filepath.Clean(volume + c.root)
			path := filepath.Clean(volume + c.path)
			rootClean, sub, ok := matchAllowedRootLiteral(path, []string{root})
			if ok != c.wantOK {
				t.Fatalf("match = %v, want %v", ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if rootClean != root {
				t.Fatalf("rootClean = %q, want %q", rootClean, root)
			}
			if sub != filepath.FromSlash(c.wantSub) {
				t.Fatalf("sub = %q, want %q", sub, c.wantSub)
			}
		})
	}
}

func TestMatchAllowedRootIgnoresEmptyRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if root, sub, ok := matchAllowedRoot(path, []string{""}); ok {
		t.Fatalf("empty allowed root matched %q as (%q, %q)", path, root, sub)
	}
}

func TestMatchFileTargetJudgesClimbingRelativePathsByTheirAbsoluteForm(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	sibling := filepath.Join(parent, "sibling")

	_, err := matchFileTarget("../sibling/notes.md", workspace, nil, "read file")
	if err == nil || !strings.Contains(err.Error(), "outside workspace and allowed roots") || !strings.Contains(err.Error(), filepath.Join(sibling, "notes.md")) {
		t.Fatalf("climbing path error = %v, want the outside-roots error naming the resolved path", err)
	}

	target, err := matchFileTarget("../sibling/notes.md", workspace, []string{sibling}, "read file")
	if err != nil || target.InWorkspace || target.RootPath != sibling || target.RelPath != "notes.md" {
		t.Fatalf("climbing path into an allowed root = %+v, %v", target, err)
	}

	target, err = matchFileTarget("sub/../notes.md", workspace, nil, "read file")
	if err != nil || !target.InWorkspace {
		t.Fatalf("path that stays inside the workspace = %+v, %v", target, err)
	}
}

func TestMatchReadTargetAdmitsAnyPathUnlessReadRootsConfineIt(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	sibling := filepath.Join(parent, "sibling")

	for _, path := range []string{"../sibling/notes.md", filepath.Join(sibling, "notes.md")} {
		target, err := matchReadTarget(path, workspace, nil, "read file")
		if err != nil || target.InWorkspace || target.RootPath != "" || target.AbsPath != filepath.Join(sibling, "notes.md") {
			t.Fatalf("unconfined %q = %+v, %v", path, target, err)
		}
		if _, err := matchReadTarget(path, workspace, []string{workspace}, "read file"); err == nil || !strings.Contains(err.Error(), "outside workspace and allowed roots") {
			t.Fatalf("%q confined to the workspace = %v, want the outside-roots error", path, err)
		}
	}

	for _, roots := range [][]string{nil, {workspace}, {sibling}} {
		target, err := matchReadTarget("sub/../notes.md", workspace, roots, "read file")
		if err != nil || !target.InWorkspace || target.AbsPath != filepath.Join(workspace, "notes.md") {
			t.Fatalf("workspace path with read roots %q = %+v, %v", roots, target, err)
		}
	}
}
