package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func copyResource(test *testing.T, app *Server, from, to string) error {
	test.Helper()
	h := app.ahpHost()
	raw, _ := json.Marshal(map[string]any{"channel": t.RootResourceURI, "source": h.fileURI(from), "destination": h.fileURI(to), "failIfExists": true})
	_, err := h.resource(ahpRequest{Method: "resourceCopy", Params: raw})
	return err
}

func TestFileCopyRejectsOwnDescendants(test *testing.T) {
	for _, tc := range []struct {
		name  string
		alias string
		to    string
	}{
		{name: "direct", to: "source/copy"},
		{name: "missing parent", to: "source/new/copy"},
		{name: "directory alias", alias: "source", to: "alias/copy"},
		{name: "descendant alias", alias: "source/child", to: "alias/copy"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			dir := test.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "source", "child"), 0755); err != nil {
				test.Fatal(err)
			}
			if tc.alias != "" {
				testenv.DirLink(test, "symlink", filepath.FromSlash(tc.alias), filepath.Join(dir, "alias"))
			}
			app := newDebugTestServer(test, dir)
			if err := copyResource(test, app, "source", tc.to); err == nil {
				test.Error("copy into its own source was accepted")
			}
			if _, err := app.workspace.Root.Lstat(tc.to); !os.IsNotExist(err) {
				test.Errorf("rejected copy created its destination: %v", err)
			}
		})
	}
}

func TestFileCopyAllowsSiblingDirectory(test *testing.T) {
	dir := test.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "file.txt"), []byte("contents"), 0644); err != nil {
		test.Fatal(err)
	}
	app := newDebugTestServer(test, dir)
	if err := copyResource(test, app, "source", "source-copy"); err != nil {
		test.Fatal(err)
	}
	content, err := app.workspace.Root.ReadFile("source-copy/file.txt")
	if err != nil || string(content) != "contents" {
		test.Fatalf("copy content = %q, error = %v", content, err)
	}
}

func TestFileCopyPreservesSymlinks(test *testing.T) {
	dir := test.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source", "nested"), 0755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "file.txt"), []byte("contents"), 0644); err != nil {
		test.Fatal(err)
	}
	links := map[string]string{"file-link": "file.txt", "directory-link": "nested", "broken-link": "missing"}
	for name, target := range links {
		testenv.DirLink(test, "symlink", target, filepath.Join(dir, "source", name))
	}
	app := newDebugTestServer(test, dir)
	for _, from := range []string{"source", "source/file-link"} {
		test.Run(from, func(test *testing.T) {
			to := from + "-copy"
			if err := copyResource(test, app, from, to); err != nil {
				test.Fatal(err)
			}
			want := links
			if from != "source" {
				want = map[string]string{".": "file.txt"}
			}
			for name, target := range want {
				got, err := app.workspace.Root.Readlink(filepath.Join(to, name))
				if err != nil || got != target {
					test.Errorf("copied link %s = %q, %v; want %q", name, got, err, target)
				}
			}
		})
	}
}
