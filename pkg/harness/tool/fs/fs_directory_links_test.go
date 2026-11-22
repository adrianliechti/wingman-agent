package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	. "github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

func TestFileToolsKeepOpenedWorkspaceBoundary(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			original, outside := t.TempDir(), t.TempDir()
			for dir, content := range map[string]string{original: "original", outside: "outside"} {
				real := filepath.Join(dir, "real")
				if err := os.Mkdir(real, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(real, "file.txt"), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
				testenv.DirLink(t, kind, real, filepath.Join(dir, "linked"))
			}
			if err := os.Mkdir(filepath.Join(outside, "private"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "private", "secret.txt"), []byte("secret"), 0644); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "workspace")
			testenv.DirLink(t, kind, original, alias)
			root, err := os.OpenRoot(alias)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			// Retargeting the path must not change an already opened workspace.
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			testenv.DirLink(t, kind, outside, alias)
			for _, check := range []struct {
				tool tool.Tool
				args map[string]any
			}{
				{ReadTool(root), map[string]any{"file_path": "private/secret.txt"}},
				{GrepTool(root), map[string]any{"path": "private", "pattern": "secret"}},
				{GlobTool(root), map[string]any{"path": "private", "pattern": "**/*.txt"}},
				{EditTool(root), createArgs("linked/new.txt", "overwrite")},
				{EditTool(root), map[string]any{"file_path": "linked/file.txt", "old_string": "outside", "new_string": "overwrite"}},
			} {
				if result, err := check.tool.Execute(context.Background(), check.args); err == nil {
					t.Errorf("%s followed the replacement workspace: %q", check.tool.Name, result.Content.Text())
				}
			}
			if data, err := os.ReadFile(filepath.Join(outside, "real", "file.txt")); err != nil || string(data) != "outside" {
				t.Fatalf("replacement workspace was modified: %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(outside, "real", "new.txt")); !os.IsNotExist(err) {
				t.Fatalf("replacement workspace gained a file: %v", err)
			}
			result, err := ReadTool(root).Execute(context.Background(), map[string]any{"file_path": "real/file.txt"})
			if err != nil || !strings.Contains(result.Content.Text(), "original") {
				t.Fatalf("original workspace is no longer readable: %q, %v", result.Content.Text(), err)
			}
		})
	}
}

func TestFileToolsDirectoryLinkPaths(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		for _, scope := range []string{"workspace", "allowed root"} {
			t.Run(kind+"/"+scope, func(t *testing.T) {
				workspace := t.TempDir()
				base := workspace
				var allowed []string
				// Confined reads name the workspace; without read roots they reach any path.
				readRoots := []string{workspace}
				if scope == "allowed root" {
					base = t.TempDir()
					allowed = []string{base}
					readRoots = allowed
				}
				real := filepath.Join(base, "real")
				if err := os.Mkdir(real, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(real, "note.txt"), []byte("linked content"), 0644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(base, "link")
				testenv.DirLink(t, kind, real, link)
				root, err := os.OpenRoot(workspace)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				file := filepath.Join(link, "note.txt")
				if _, err := EditTool(root, allowed...).Execute(context.Background(), createArgs(filepath.Join(link, "missing", "nested", "output.txt"), "linked output")); err != nil {
					t.Errorf("create through link with missing parents: %v", err)
				}
				if data, err := os.ReadFile(filepath.Join(real, "missing", "nested", "output.txt")); err != nil || string(data) != "linked output" {
					t.Errorf("create did not reach the contained directory: %q, %v", data, err)
				}
				for _, roots := range [][]string{readRoots, nil} {
					for _, check := range []struct {
						tool tool.Tool
						args map[string]any
					}{
						{ReadTool(root, roots...), map[string]any{"file_path": file}},
						{GrepTool(root, roots...), map[string]any{"path": link, "pattern": "linked", "output_mode": "content"}},
						{GrepTool(root, roots...), map[string]any{"path": file, "pattern": "linked", "output_mode": "content"}},
						{GlobTool(root, roots...), map[string]any{"path": link, "pattern": "**/*.txt"}},
					} {
						result, err := check.tool.Execute(context.Background(), check.args)
						if err != nil {
							t.Errorf("%s through link with read roots %q: %v", check.tool.Name, roots, err)
						} else if check.tool.Name == "read" && !strings.Contains(result.Content.Text(), "linked content") {
							t.Errorf("read did not reach the linked file: %q", result.Content.Text())
						} else if check.tool.Name != "read" && !strings.Contains(result.Content.Text(), filepath.Join("link", "note.txt")) {
							t.Errorf("%s with read roots %q lost the linked file's display path: %q", check.tool.Name, roots, result.Content.Text())
						}
					}
				}

				outside := t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644); err != nil {
					t.Fatal(err)
				}
				escape := filepath.Join(base, "escape")
				testenv.DirLink(t, kind, outside, escape)
				for _, check := range []struct {
					tool tool.Tool
					args map[string]any
				}{
					{ReadTool(root, readRoots...), map[string]any{"file_path": filepath.Join(escape, "secret.txt")}},
					{GrepTool(root, readRoots...), map[string]any{"path": escape, "pattern": "secret"}},
					{GlobTool(root, readRoots...), map[string]any{"path": escape, "pattern": "**/*.txt"}},
					{EditTool(root, allowed...), createArgs(filepath.Join(escape, "missing", "output.txt"), "overwrite")},
				} {
					if result, err := check.tool.Execute(context.Background(), check.args); err == nil {
						t.Errorf("%s followed an escaping directory link: %q", check.tool.Name, result.Content.Text())
					}
				}
				// Without read roots the link is an ordinary way to name the outside directory.
				for _, check := range []struct {
					tool tool.Tool
					args map[string]any
				}{
					{ReadTool(root), map[string]any{"file_path": filepath.Join(escape, "secret.txt")}},
					{GrepTool(root), map[string]any{"path": escape, "pattern": "secret"}},
					{GlobTool(root), map[string]any{"path": escape, "pattern": "**/*.txt"}},
				} {
					if result, err := check.tool.Execute(context.Background(), check.args); err != nil || !strings.Contains(result.Content.Text(), "secret") {
						t.Errorf("unconfined %s through an escaping directory link = %q, %v", check.tool.Name, result.Content.Text(), err)
					}
				}
				if _, err := os.Stat(filepath.Join(outside, "missing")); !os.IsNotExist(err) {
					t.Errorf("escaping create made a directory: %v", err)
				}
			})
		}
	}
}

func TestCatalogReadsExplicitFileAliasInWriteRoot(t *testing.T) {
	workspace, writable, sibling := t.TempDir(), t.TempDir(), t.TempDir()
	note := filepath.Join(sibling, "notes.txt")
	if err := os.WriteFile(note, []byte("ordinary notes"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(writable, "alias.md")
	if err := os.Symlink(note, alias); err != nil {
		t.Skipf("file links unavailable: %v", err)
	}
	for _, tl := range Tools(openRoot(t, workspace), &Options{AllowedWriteRoots: []string{writable}}) {
		var args map[string]any
		switch tl.Name {
		case "read":
			args = map[string]any{"file_path": alias}
		case "grep":
			args = map[string]any{"path": alias, "pattern": "ordinary", "glob": "*.md", "output_mode": "content"}
		default:
			continue
		}
		out, err := tl.Execute(t.Context(), args)
		if err != nil || !strings.Contains(out.Content.Text(), "ordinary notes") || (tl.Name == "grep" && !strings.Contains(out.Content.Text(), alias)) {
			t.Errorf("%s on alias = %q, %v", tl.Name, out.Content.Text(), err)
		}
	}
}
