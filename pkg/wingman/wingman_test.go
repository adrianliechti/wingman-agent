package wingman_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/adrianliechti/wingman-agent/pkg/wingman"
)

func TestFromEnv(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")
	t.Setenv("WINGMAN_TOKEN", "token")

	client, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv error: %v", err)
	}
	if client.BaseURL != "https://wingman.example" || client.Token != "token" {
		t.Fatalf("client = %#v", client)
	}
}

func TestFromEnvRequiresBaseURL(t *testing.T) {
	t.Setenv("WINGMAN_URL", "")

	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "WINGMAN_URL") {
		t.Fatalf("expected WINGMAN_URL error, got: %v", err)
	}
}

func TestClientFetchSendsRequestAndReportsTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMultipartRequest(t, r, "/v1/extract")
		if got := r.FormValue("url"); got != "https://example.com" {
			t.Fatalf("url = %q", got)
		}
		if got := r.FormValue("prompt"); got != "extract" {
			t.Fatalf("prompt = %q", got)
		}

		_, _ = io.WriteString(w, strings.Repeat("a", MaxFetchBytes+1))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL + "/", Token: "token"}
	content, truncated, err := client.Fetch(context.Background(), "https://example.com", "extract")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(content) != MaxFetchBytes {
		t.Fatalf("content length = %d, want %d", len(content), MaxFetchBytes)
	}
}

func TestClientSearchStructuredResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMultipartRequest(t, r, "/v1/search")
		if got := r.FormValue("query"); got != "golang" {
			t.Fatalf("query = %q", got)
		}
		if got := r.FormValue("allowed_domains"); got != `["go.dev"]` {
			t.Fatalf("allowed_domains = %q", got)
		}

		_, _ = io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"Docs"}]}`)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "token"}
	results, text, err := client.Search(context.Background(), "golang", []string{"go.dev"}, nil)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if len(results) != 1 || results[0].Title != "Go" || results[0].URL != "https://go.dev" {
		t.Fatalf("results = %#v", results)
	}
}

func TestClientSearchTextFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain results")
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	results, text, err := client.Search(context.Background(), "golang", nil, nil)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 0 || text != "plain results" {
		t.Fatalf("results=%#v text=%q", results, text)
	}
}

func TestClientHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadGateway)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	_, _, err := client.Fetch(context.Background(), "https://example.com", "extract")
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("expected HTTP status error, got: %v", err)
	}
}

func assertMultipartRequest(t *testing.T, r *http.Request, path string) {
	t.Helper()

	if r.URL.Path != path {
		t.Fatalf("path = %q, want %s", r.URL.Path, path)
	}
	if r.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("authorization = %q, want bearer token", got)
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
		t.Fatalf("content type = %q, want multipart/form-data", r.Header.Get("Content-Type"))
	}
	if err := r.ParseMultipartForm(1024); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
}
