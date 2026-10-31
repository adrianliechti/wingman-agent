package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestWorkspaceContentSearchStreamsFilteredReplaceableMatches(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		".gitignore":           "ignored.txt\n",
		"notes.txt":            "Alpha alpha cat scatter 😀cat\n",
		"ignored.txt":          "cat\n",
		"src/code.go":          "const cat = cat\n",
		"build/out.txt":        "cat\n",
		"nested/.gitignore":    "skipped.txt\n",
		"nested/skipped.txt":   "cat\n",
		"nested/not-text.data": "cat\n",
	} {
		if err := os.WriteFile(filepath.Join(workDir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	app := &Server{workspace: &code.Workspace{Root: root, RootPath: workDir}}

	var events []workspaceSearchEvent
	err = app.searchWorkspace(t.Context(), workspaceSearchRequest{
		Query:         "cat",
		Replacement:   "dog",
		CaseSensitive: true,
		WholeWord:     true,
		Include:       "*.txt",
	}, func(event workspaceSearchEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || len(events) != 2 {
		t.Fatalf("search events = %#v, err = %v", events, err)
	}
	fileEvent := events[0]
	if fileEvent.Type != "file" || fileEvent.File == nil || fileEvent.File.Path != "notes.txt" {
		t.Fatalf("file event = %#v", fileEvent)
	}
	if fileEvent.File.Revision == "" || len(fileEvent.File.Matches) != 2 {
		t.Fatalf("search file = %#v", fileEvent.File)
	}
	first, second := fileEvent.File.Matches[0], fileEvent.File.Matches[1]
	if first.Line != 1 || first.Column != 13 || first.EndColumn != 16 || first.Replacement != "dog" {
		t.Fatalf("first match = %#v", first)
	}
	if second.Column != 27 || second.EndColumn != 30 {
		t.Fatalf("unicode match columns = %#v", second)
	}

	done := events[1]
	if done.Type != "done" || done.Files != 1 || done.Matches != 2 || done.Truncated {
		t.Fatalf("done event = %#v", done)
	}
}

func TestWorkspaceContentSearchRegexReplacementAndValidation(t *testing.T) {
	config, err := newWorkspaceSearchConfig(workspaceSearchRequest{
		Query:         "(cat)",
		Replacement:   "${1}s",
		Regex:         true,
		CaseSensitive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, more := config.searchFile("cat scatter cat", 10)
	if more || len(matches) != 3 {
		t.Fatalf("matches = %#v, more = %v", matches, more)
	}
	if matches[0].Replacement != "cats" {
		t.Fatalf("expanded replacement = %q", matches[0].Replacement)
	}

	if _, err := newWorkspaceSearchConfig(workspaceSearchRequest{Query: "(", Regex: true}); err == nil {
		t.Fatal("invalid regex was accepted")
	}
	if _, err := newWorkspaceSearchConfig(workspaceSearchRequest{Query: "cat", Include: "["}); err == nil {
		t.Fatal("invalid glob was accepted")
	}
}

func TestFilePreviewServesWebsiteAssetsInline(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	workDir := t.TempDir()
	siteDir := filepath.Join(workDir, "site")
	if err := os.MkdirAll(filepath.Join(siteDir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html":      `<!doctype html><link rel="stylesheet" href="style.css"><img src="images/logo.svg">`,
		"style.css":       "body { color: rebeccapurple; }",
		"images/logo.svg": `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(siteDir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "secret.txt"), []byte("outside preview root"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(context.Background(), workDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	tests := []struct {
		path        string
		contentType string
		body        string
	}{
		{"/preview?path=site%2Findex.html", "text/html", "<!doctype html>"},
		{"/style.css", "text/css", "rebeccapurple"},
		{"/images/logo.svg", "image/svg+xml", "<svg"},
	}
	for i, test := range tests {
		requestURL := app.preview.URL() + test.path
		if i == 0 {
			requestURL = web.URL + test.path
		}
		res, err := client.Get(requestURL)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %q", test.path, res.StatusCode, body)
		}
		if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, test.contentType) {
			t.Errorf("%s: content type = %q, want prefix %q", test.path, got, test.contentType)
		}
		if got := res.Header.Get("Content-Disposition"); got != "" {
			t.Errorf("%s: content disposition = %q, want inline response", test.path, got)
		}
		if !strings.Contains(string(body), test.body) {
			t.Errorf("%s: body = %q, want it to contain %q", test.path, body, test.body)
		}
	}

	res, err := client.Get(app.preview.URL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("directory index status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, "form-action 'none'") {
		t.Fatalf("content security policy = %q", got)
	}

	res, err = client.Get(app.preview.URL() + "/%2e%2e%5csecret.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	res.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if res.StatusCode != http.StatusNotFound || strings.Contains(string(body), "outside preview root") {
		t.Fatalf("preview root escape: status = %d, body = %q", res.StatusCode, body)
	}
}
