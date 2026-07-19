package websearch

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestParseResults(t *testing.T) {
	page, err := os.ReadFile("testdata/results.html")
	if err != nil {
		t.Fatal(err)
	}

	results := parseResults(string(page))
	if len(results) == 0 {
		t.Fatal("no results parsed from fixture")
	}

	for i, r := range results {
		if r.title == "" {
			t.Fatalf("result %d has empty title", i)
		}
		if !strings.HasPrefix(r.url, "http") {
			t.Fatalf("result %d url = %q, want resolved http(s) URL", i, r.url)
		}
		if strings.Contains(r.url, "duckduckgo.com") {
			t.Fatalf("result %d url = %q, want redirect unwrapped", i, r.url)
		}
	}

	if results[0].snippet == "" {
		t.Fatal("first result has no snippet")
	}
}

func TestResolveResultURL(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgithub.com%2Ftree%2Dsitter%2Fgo%2Dtree%2Dsitter&rut=abc": "https://github.com/tree-sitter/go-tree-sitter",
		"https://example.com/page":                    "https://example.com/page",
		"//duckduckgo.com/y.js?ad_domain=example.com": "",
		"javascript:alert(1)":                         "",
		"":                                            "",
	}

	for href, want := range cases {
		if got := resolveResultURL(href); got != want {
			t.Fatalf("resolveResultURL(%q) = %q, want %q", href, got, want)
		}
	}
}

func TestSearchApprovalGate(t *testing.T) {
	var prompts int
	allow := false
	elicit := &tool.Elicitation{Confirm: func(_ context.Context, _ string) (bool, error) {
		prompts++
		return allow, nil
	}}
	tl := Tools(elicit)[0]

	if _, err := tl.Execute(t.Context(), map[string]any{"query": "anything"}); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("declined search err = %v", err)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d, want 1", prompts)
	}
}

func TestExecuteRejectsBadInput(t *testing.T) {
	tl := Tools(nil)[0]

	if _, err := tl.Execute(t.Context(), map[string]any{}); err == nil {
		t.Fatal("missing query should error")
	}
	if _, err := tl.Execute(t.Context(), map[string]any{"query": "x", "max_results": -1}); err == nil {
		t.Fatal("negative max_results should error")
	}
}
