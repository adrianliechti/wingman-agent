package codex

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFileCitationsAcrossEveryStreamBoundary(t *testing.T) {
	cwd := t.TempDir()
	for _, tt := range []struct{ input, want string }{
		{`See :codex-file-citation{path="docs/policy.txt" purpose="source"}.`, "See [docs/policy.txt](" + filepath.Join(cwd, "docs/policy.txt") + ")."},
		{`:::codex-file-citation{path = 'a b.txt'}`, "[a b.txt](<" + filepath.Join(cwd, "a b.txt") + ">)"},
		{`:codex-file-citation{path="C:\src\file.txt"}`, `[C:\src\file.txt](C:\src\file.txt)`},
		{`:codex-file-citation{path=https://example.com/source}`, `[https://example.com/source](https://example.com/source)`},
		{`:codex-file-citation{path="unfinished`, `:codex-file-citation{path="unfinished`},
		{`:codex-file-citation{path=x path=y}`, `:codex-file-citation{path=x path=y}`},
		{`\:codex-file-citation{path=x}`, `\:codex-file-citation{path=x}`},
		{"`:codex-file-citation{path=x}`", "`:codex-file-citation{path=x}`"},
		{"```\n:codex-file-citation{path=x}\n```", "```\n:codex-file-citation{path=x}\n```"},
	} {
		for split := 0; split <= len(tt.input); split++ {
			stream := citationStream{cwd: cwd}
			got := stream.push(tt.input[:split]) + stream.push(tt.input[split:]) + stream.flush()
			if got != tt.want {
				t.Fatalf("%q split at %d: got %q, want %q", tt.input, split, got, tt.want)
			}
		}
		if got := renderCitations(tt.input, cwd); got != tt.want {
			t.Fatalf("replay %q: %q", tt.input, got)
		}
	}
}

func TestFileCitationBufferIsBounded(t *testing.T) {
	stream := citationStream{}
	input := `:codex-file-citation{path="` + strings.Repeat("x", 8192)
	var output strings.Builder
	for _, b := range []byte(input) {
		output.WriteString(stream.push(string(b)))
		if len(stream.pending) > 4096 {
			t.Fatal("unfinished citation exceeded the buffer limit")
		}
	}
	output.WriteString(stream.flush())
	if output.String() != input {
		t.Fatal("malformed citation lost text")
	}
}
