package markdown

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestFileReferencesAndMultilineLinks(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "main file.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	want := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), Fragment: "L42"}).String()
	for _, source := range []string{"[source](<" + path + ":42>)", "`main file.go:42`", "[source](<main%20file.go#L42>)"} {
		got := Render(source, Options{Directory: directory})
		if !strings.Contains(got, "\x1b]8;;"+want+"\x1b\\") {
			t.Fatalf("%q did not link to %q: %q", source, want, got)
		}
	}
	const destination = "https://example.com/source"
	got := Render("[before `main file.go`\nafter]("+destination+")", Options{Directory: directory})
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Count(line, "\x1b]8;;"+destination+"\x1b\\") != 1 || !strings.Contains(line, ansi.HyperlinkEnd) {
			t.Fatalf("nested code or a line break lost the outer link: %q", got)
		}
	}
	if strings.Contains(got, "file://") {
		t.Fatal("inline code replaced its enclosing link")
	}
	for _, source := range []string{"`missing.go:12`", "`fmt.Println()`", "[bad](javascript:alert)"} {
		if got := Render(source, Options{Directory: directory}); strings.Contains(got, "\x1b]8;") {
			t.Fatalf("ordinary code or unsupported URL became clickable: %q", got)
		}
	}
}

func TestTablesAdaptToContentWidthWithoutDroppingValues(t *testing.T) {
	const source = "| Name | Description | Status |\n| :--- | :--- | ---: |\n| **alpha** | useful details about the change | ready |\n| 日本語 | [source](https://example.com) | pending |\n"
	for _, width := range []int{100, 60, 35, 20, 8} {
		rendered := Render(source, Options{Width: width})
		for _, line := range strings.Split(rendered, "\n") {
			if ansi.Width(line) > width {
				t.Fatalf("width %d overflowed with %q (%d columns)", width, line, ansi.Width(line))
			}
		}
		plain := ansi.Strip(rendered)
		compact := strings.Join(strings.Fields(plain), "")
		for _, value := range []string{"alpha", "日本語", "pending", "source"} {
			if !strings.Contains(compact, value) {
				t.Errorf("width %d dropped %q: %q", width, value, plain)
			}
		}
		if strings.Contains(plain, "**") || !strings.Contains(rendered, "\x1b]8;;https://example.com") {
			t.Fatalf("width %d lost inline formatting/links: %q", width, rendered)
		}
		if width == 100 && !strings.Contains(plain, "│") {
			t.Fatal("wide table did not use columns")
		}
		if width <= 20 && strings.Contains(plain, "│") {
			t.Fatal("narrow table did not use labeled records")
		}
	}
}

func TestCopyTargetsUseOriginalCodeAndQuoteText(t *testing.T) {
	const source = "Intro\n\n```go\n\tprintln(\"日本語\")  \n\n```\n\n> **Keep** `these` words.\n> Next line.\n\n    indented code\n"
	targets := CopyTargets(source)
	if len(targets) != 3 {
		t.Fatalf("copy targets = %+v", targets)
	}
	if targets[0].Text != "\tprintln(\"日本語\")  \n\n" || !strings.Contains(targets[0].Label, "go") {
		t.Fatalf("code whitespace/language changed: %+v", targets[0])
	}
	if targets[1].Text != "Keep these words.\nNext line." {
		t.Fatalf("quote text = %q", targets[1].Text)
	}
	if targets[2].Text != "indented code\n" {
		t.Fatalf("indented code text = %q", targets[2].Text)
	}
	if targets := CopyTargets("```sh\nprintf 'streaming'\n"); len(targets) != 1 || targets[0].Text != "printf 'streaming'\n" {
		t.Fatalf("open code fence could not be copied: %+v", targets)
	}
}
