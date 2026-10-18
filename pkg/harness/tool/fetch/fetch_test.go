package fetch

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func fetchTool(t *testing.T) func(map[string]any) (string, error) {
	t.Helper()
	tl := Tools(nil)[0]
	return func(args map[string]any) (string, error) {
		result, err := tl.Execute(t.Context(), args)
		return result.Content, err
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

	for _, want := range []string{"# Guide", "[continue](" + server.URL + "/next)", "- one", "- two"} {
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

func TestFetchHTMLTitleStrikeAndRemovedTags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Docs</title></head><body><p>old <del>gone</del></p><svg><text>icon-noise</text></svg></body></html>`))
	}))
	defer server.Close()

	out, err := fetchTool(t)(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "# Docs") || !strings.Contains(out, "~~gone~~") || strings.Contains(out, "icon-noise") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestFetchDecodesHTMLCharset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		w.Write([]byte("<html><body><p>caf\xe9</p></body></html>"))
	}))
	defer server.Close()

	out, err := fetchTool(t)(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "café") {
		t.Fatalf("output = %q", out)
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
	if !strings.Contains(out, "[truncated at 47KB]") {
		t.Fatal("missing truncation notice")
	}
}

func TestFetchHostApprovalGate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	var prompts int
	allow := false
	elicit := &tool.Elicitation{Confirm: func(_ context.Context, _ string) (bool, error) {
		prompts++
		return allow, nil
	}}
	tl := Tools(elicit)[0]

	if _, err := tl.Execute(t.Context(), map[string]any{"url": server.URL}); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("declined fetch err = %v", err)
	}

	allow = true
	if result, err := tl.Execute(t.Context(), map[string]any{"url": server.URL}); err != nil || result.Content != "ok" {
		t.Fatalf("approved fetch = %q, %v", result.Content, err)
	}
	if _, err := tl.Execute(t.Context(), map[string]any{"url": server.URL + "/again"}); err != nil {
		t.Fatalf("remembered approval fetch err = %v", err)
	}
	if prompts != 2 {
		t.Fatalf("prompts = %d, want 2 (decline + approve, then remembered)", prompts)
	}
}

func TestFetchExtractsBinaryDocuments(t *testing.T) {
	pdf := buildTestPDF("Fetched PDF Guide")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(pdf)
	}))
	defer server.Close()

	out, err := fetchTool(t)(map[string]any{"url": server.URL + "/guide.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Fetched PDF Guide") {
		t.Fatalf("output = %q, want Fetched PDF Guide", out)
	}
}

func buildTestPDF(text string) []byte {
	content := fmt.Sprintf("BT\n/F1 18 Tf\n72 720 Td\n(%s) Tj\nET\n", strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(text))
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, object := range objects {
		offsets[i+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return pdf.Bytes()
}
