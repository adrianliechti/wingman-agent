//go:build unix

package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewWorkspaceContentConfinesAndStreams(t *testing.T) {
	app, root := reviewWorkspaceApp(t, map[string]string{"a.txt": "0123456789", "dir/index.html": "<p>x</p>"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "secret-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	web := httptest.NewServer(scopedTestHandler(app))
	t.Cleanup(web.Close)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(route, path string, header http.Header) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", web.URL+route+"?path="+url.QueryEscape(path), nil)
		for key, values := range header {
			req.Header[key] = values
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", route, path, err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	for _, route := range []string{"/content", "/preview"} {
		for path, want := range map[string]int{
			"": 400, ".": 400, "../x": 400, "/etc/hosts": 400, outside + "/secret": 400, "a.txt/../../x": 400,
			"escape/secret": 404, "secret-link": 400, "pipe": 400, "dir": 400, "missing": 404, "a.txt\x00": 404,
		} {
			res := get(route, path, nil)
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != want || bytes.Contains(body, []byte("secret")) && path != "escape/secret" {
				t.Fatalf("%s %q: %d %s", route, path, res.StatusCode, body)
			}
		}
	}
	res := get("/content", "a.txt", http.Header{"Range": {"bytes=2-4"}})
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "234" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("range: %d %q %v", res.StatusCode, body, res.Header)
	}
	res = get("/preview", "dir/index.html", nil)
	location, err := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusTemporaryRedirect || err != nil || location.Hostname() != "127.0.0.1" || location.Path != "/__start" {
		t.Fatalf("preview: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	name := "we\"ird\r\nX-Injected: yes.txt"
	if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err == nil {
		res = get("/content", name, nil)
		if res.StatusCode != http.StatusOK || res.Header.Get("X-Injected") != "" || strings.ContainsAny(res.Header.Get("Content-Disposition"), "\r\n") {
			t.Fatalf("filename header: %d %v", res.StatusCode, res.Header)
		}
	}
}
