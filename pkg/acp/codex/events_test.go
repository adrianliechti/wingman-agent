package codex

import (
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestFileChangeContentAddUnifiedDiff(t *testing.T) {
	raw := []byte(`{"changes":[{"path":"/p/NewFile.kt","kind":{"type":"add"},"diff":"--- /dev/null\n+++ /p/NewFile.kt\n@@ -0,0 +1,3 @@\n+package test\n+\n+class NewFile {}"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	d := content[0].Diff
	if d.Path != "/p/NewFile.kt" {
		t.Errorf("path = %q", d.Path)
	}
	if d.OldText != nil {
		t.Errorf("add should have nil oldText, got %q", *d.OldText)
	}
	if want := "package test\n\nclass NewFile {}"; d.NewText != want {
		t.Errorf("newText = %q, want %q", d.NewText, want)
	}
}

func TestFileChangeContentAddRawContent(t *testing.T) {

	raw := []byte(`{"changes":[{"path":"/p/Raw.kt","kind":{"type":"add"},"diff":"fun main() {}\n"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	if got := content[0].Diff.NewText; got != "fun main() {}\n" {
		t.Errorf("newText = %q", got)
	}
	if content[0].Diff.OldText != nil {
		t.Errorf("add should have nil oldText")
	}
}

func TestFileChangeContentUpdate(t *testing.T) {
	raw := []byte(`{"changes":[{"path":"/p/a.go","kind":{"type":"update"},"diff":"--- a/p/a.go\n+++ b/p/a.go\n@@ -1,3 +1,3 @@\n line one\n-old line\n+new line\n line three"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	d := content[0].Diff
	if d.OldText == nil || *d.OldText != "line one\nold line\nline three" {
		t.Errorf("oldText = %v", d.OldText)
	}
	if d.NewText != "line one\nnew line\nline three" {
		t.Errorf("newText = %q", d.NewText)
	}
}

func TestCommandActionToolCall(t *testing.T) {
	title, kind, locs, ok := commandActionToolCall([]commandAction{{Type: "read", Path: "/x"}})
	if !ok || title != "Read file '/x'" || kind != acp.ToolKindRead || len(locs) != 1 || locs[0].Path != "/x" {
		t.Errorf("read: got %q %v %v %v", title, kind, locs, ok)
	}

	title, kind, _, ok = commandActionToolCall([]commandAction{{Type: "search", Query: "foo", Path: "/p"}})
	if !ok || title != "Search for 'foo' in /p" || kind != acp.ToolKindSearch {
		t.Errorf("search: got %q %v %v", title, kind, ok)
	}

	title, kind, _, ok = commandActionToolCall([]commandAction{{Type: "listFiles", Path: "/p"}})
	if !ok || title != "List files in '/p'" || kind != acp.ToolKindRead {
		t.Errorf("listFiles: got %q %v %v", title, kind, ok)
	}
	if title, _, _, _ := commandActionToolCall([]commandAction{{Type: "listFiles"}}); title != "List files" {
		t.Errorf("listFiles no path: got %q", title)
	}

	if _, _, _, ok := commandActionToolCall([]commandAction{{Type: "read", Path: "/x"}, {Type: "read", Path: "/y"}}); ok {
		t.Errorf("multiple actions should not resolve to a single mapping")
	}
	if _, _, _, ok := commandActionToolCall([]commandAction{{Type: "unknown"}}); ok {
		t.Errorf("unknown action should not resolve")
	}
}

func TestSearchTitle(t *testing.T) {
	cases := map[string][2]string{
		"Search for 'q' in /p": {"q", "/p"},
		"Search for 'q'":       {"q", ""},
		"Search in '/p'":       {"", "/p"},
		"Search":               {"", ""},
	}
	for want, in := range cases {
		if got := searchTitle(in[0], in[1]); got != want {
			t.Errorf("searchTitle(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestStripShellPrefix(t *testing.T) {
	cases := map[string]string{
		"/bin/zsh -c npm install":          "npm install",
		"/bin/bash -lc npm install":        "npm install",
		"zsh npm install":                  "npm install",
		"sh -c ls -la":                     "ls -la",
		"npm install":                      "npm install",
		"/bin/bash -lc './tests.cmd -D=v'": "./tests.cmd -D=v",
		"/bin/zsh -c 'echo hello'":         "echo hello",
	}
	for in, want := range cases {
		if got := stripShellPrefix(in); got != want {
			t.Errorf("stripShellPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileChangeContentDeleteMeta(t *testing.T) {
	raw := []byte(`{"changes":[{"path":"/p/gone.go","kind":{"type":"delete"},"diff":"--- a/p/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-line one\n-line two"}]}`)
	content := fileChangeContent(raw)
	if len(content) != 1 || content[0].Diff == nil {
		t.Fatalf("expected one diff block, got %#v", content)
	}
	d := content[0].Diff
	if d.Meta["kind"] != "delete" {
		t.Errorf("meta kind = %v, want delete", d.Meta["kind"])
	}
	if d.NewText != "" {
		t.Errorf("delete newText = %q, want empty", d.NewText)
	}
	if d.OldText == nil || *d.OldText != "line one\nline two" {
		t.Errorf("delete oldText = %v", d.OldText)
	}
}

func TestIsFatalTurnError(t *testing.T) {
	cases := []struct {
		name string
		info string
		want bool
	}{
		{"unauthorized string", `"unauthorized"`, true},
		{"usage limit string", `"usageLimitExceeded"`, true},
		{"other string", `"somethingElse"`, false},
		{"http 401 object", `{"httpConnectionFailed":{"httpStatusCode":401}}`, true},
		{"stream disconnected 401", `{"responseStreamDisconnected":{"httpStatusCode":401}}`, true},
		{"http 500 object", `{"httpConnectionFailed":{"httpStatusCode":500}}`, false},
		{"empty", ``, false},
		{"null", `null`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isFatalTurnError([]byte(c.info)); got != c.want {
				t.Errorf("isFatalTurnError(%s) = %v, want %v", c.info, got, c.want)
			}
		})
	}
}
