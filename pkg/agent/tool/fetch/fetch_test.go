package fetch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fetchTool(t *testing.T) func(map[string]any) (string, error) {
	t.Helper()
	tl := Tools()[0]
	return func(args map[string]any) (string, error) {
		return tl.Execute(t.Context(), args)
	}
}

func TestFetchConvertsHTML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Docs</title><style>body{}</style><script>alert(1)</script></head>
<body><h1>Guide</h1><p>Hello <a href="/next">continue</a></p><ul><li>one</li><li>two</li></ul></body></html>`))
	}))
	defer server.Close()

	out, err := fetchTool(t)(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"# Guide", "[continue](/next)", "- one", "- two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"alert(1)", "body{}", "<p>"} {
		if strings.Contains(out, banned) {
			t.Fatalf("output leaked %q:\n%s", banned, out)
		}
	}
}

func TestFetchPlainTextPassthrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	out, err := fetchTool(t)(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Fatalf("output = %q", out)
	}
}

func TestFetchRejectsBinaryAndBadInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0x00, 0x01, 0x02})
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	run := fetchTool(t)

	if _, err := run(map[string]any{"url": server.URL + "/bin"}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary fetch err = %v", err)
	}
	if _, err := run(map[string]any{"url": server.URL + "/missing"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 fetch err = %v", err)
	}
	if _, err := run(map[string]any{"url": "file:///etc/passwd"}); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("scheme err = %v", err)
	}
	if _, err := run(map[string]any{}); err == nil {
		t.Fatal("missing url should error")
	}
}

func TestFetchTruncatesLongOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for range 20000 {
			w.Write([]byte("some repeated line of text\n"))
		}
	}))
	defer server.Close()

	out, err := fetchTool(t)(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutputBytes+100 {
		t.Fatalf("output len = %d, want <= %d", len(out), maxOutputBytes+100)
	}
	if !strings.Contains(out, "[truncated at 48KB]") {
		t.Fatal("missing truncation notice")
	}
}
