package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestFileCopyRejectsOwnDescendants(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "source", "child"), 0755); err != nil {
				t.Fatal(err)
			}
			if tc.alias != "" {
				testenv.DirLink(t, "symlink", filepath.FromSlash(tc.alias), filepath.Join(dir, "alias"))
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			s := &Server{workspace: &code.Workspace{Root: root, RootPath: dir}}
			destination := tc.to
			body, _ := json.Marshal(map[string]string{"from": "source", "to": destination})
			response := httptest.NewRecorder()
			s.handleFileCopy(response, httptest.NewRequest(http.MethodPost, "/api/files/copy", bytes.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Errorf("copy into source status = %d, want 400", response.Code)
			}
			if _, err := root.Lstat(destination); !os.IsNotExist(err) {
				t.Errorf("rejected copy created its destination: %v", err)
			}
		})
	}
}

func TestFileCopyAllowsSiblingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "file.txt"), []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s := &Server{workspace: &code.Workspace{Root: root, RootPath: dir}}
	response := httptest.NewRecorder()
	s.handleFileCopy(response, httptest.NewRequest(http.MethodPost, "/api/files/copy", bytes.NewBufferString(`{"from":"source","to":"source-copy"}`)))
	if response.Code != http.StatusNoContent {
		t.Fatalf("copy status = %d, want 204: %s", response.Code, response.Body.String())
	}
	content, err := root.ReadFile("source-copy/file.txt")
	if err != nil || string(content) != "contents" {
		t.Fatalf("copy content = %q, error = %v", content, err)
	}
}

func TestFileCopyPreservesSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "source", "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source", "file.txt"), []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"file-link": "file.txt", "directory-link": "nested", "broken-link": "missing"}
	for name, target := range links {
		testenv.DirLink(t, "symlink", target, filepath.Join(dir, "source", name))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s := &Server{workspace: &code.Workspace{Root: root, RootPath: dir}}
	for _, from := range []string{"source", "source/file-link"} {
		t.Run(from, func(t *testing.T) {
			to := from + "-copy"
			body, _ := json.Marshal(map[string]string{"from": from, "to": to})
			response := httptest.NewRecorder()
			s.handleFileCopy(response, httptest.NewRequest(http.MethodPost, "/api/files/copy", bytes.NewReader(body)))
			if response.Code != http.StatusNoContent {
				t.Fatalf("copy status = %d: %s", response.Code, response.Body.String())
			}
			want := links
			if from != "source" {
				want = map[string]string{".": "file.txt"}
			}
			for name, target := range want {
				got, err := root.Readlink(filepath.Join(to, name))
				if err != nil || got != target {
					t.Errorf("copied link %s = %q, %v; want %q", name, got, err, target)
				}
			}
		})
	}
}
