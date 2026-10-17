package fs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func grepReviewRoot(t *testing.T, files map[string]string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	for path, contents := range files {
		if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGrepCancellationDuringBufferedScan(t *testing.T) {
	for _, mode := range []string{"files_with_matches", "count", "content"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			checking := &grepCancelContext{Context: ctx, cancel: cancel}
			fsys := fstest.MapFS{"file": &fstest.MapFile{Data: []byte(strings.Repeat("line without a match\n", 1000))}}
			cfg, err := parseGrepArgs(map[string]any{"pattern": "needle", "output_mode": mode})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "files_with_matches":
				_, err = fileHasMatch(checking, fsys, "file", cfg.re, false)
			case "count":
				_, err = countFileMatches(checking, fsys, "file", cfg.re, false)
			case "content":
				err = searchFileWithContext(checking, fsys, "file", "file", cfg, func(string) bool { return true })
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want cancellation", err)
			}
		})
	}
}

type grepCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *grepCancelContext) Err() error {
	c.checks++
	if c.checks == 20 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestGrepBoundsBufferedFileReads(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": strings.Repeat("x", maxGrepBufferedBytes+1)})
	for _, args := range []map[string]any{
		{"multiline": true},
		{"multiline": true, "output_mode": "count"},
		{"multiline": true, "output_mode": "content"},
		{"-C": 1, "output_mode": "content"},
	} {
		args["path"], args["pattern"] = "file", "needle"
		_, err := GrepTool(root).Execute(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "10 MiB") {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestGrepReportsUnreadableIgnoreRules(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{".gitignore": "ignored\n", "ignored": "needle"})
	if err := root.Chmod(".gitignore", 0); err != nil {
		t.Fatal(err)
	}
	defer root.Chmod(".gitignore", 0644)
	if _, err := root.ReadFile(".gitignore"); err == nil {
		t.Skip("file permissions are not enforced")
	}
	_, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle"})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v", err)
	}
}

func TestGrepDoesNotReadIgnoredDirectoryRules(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{".gitignore": "ignored/\n", "ignored/.gitignore": "!**\n", "ignored/file": "needle", "ignored/sub/file": "needle", "keep": "needle"})
	if err := root.Chmod("ignored/.gitignore", 0); err != nil {
		t.Fatal(err)
	}
	defer root.Chmod("ignored/.gitignore", 0644)
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle"})
	if err != nil || !strings.Contains(result.Content, "keep") || strings.Contains(result.Content, "ignored") {
		t.Fatalf("result = %q, %v", result.Content, err)
	}
	for _, searchPath := range []string{"ignored", "ignored/sub"} {
		subResult, err := GrepTool(root).Execute(t.Context(), map[string]any{"path": searchPath, "pattern": "needle"})
		if err != nil || subResult.Content != "No files found" {
			t.Fatalf("search %s = %q, %v", searchPath, subResult.Content, err)
		}
	}
}

func TestGrepSkipsVCSNestedSubdirectorySearch(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{".git/objects/pack": "needle", "src/.svn/pristine/blob": "needle"})
	for _, searchPath := range []string{".git", ".git/objects", "src/.svn/pristine"} {
		result, err := GrepTool(root).Execute(t.Context(), map[string]any{"path": searchPath, "pattern": "needle"})
		if err != nil || result.Content != "No files found" {
			t.Fatalf("search %s = %q, %v", searchPath, result.Content, err)
		}
	}
}

func TestGrepGlobRelativeDepthFilter(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file.go": "needle", "nested/file.go": "needle"})
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle", "glob": "./file.go"})
	if err != nil || result.Content != "Found 1 file\nfile.go" {
		t.Fatalf("result = %q, %v", result.Content, err)
	}
}

func TestGrepRootIgnoreRulesCanReincludeFiles(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{".gitignore": "*\n!keep\n", "keep": "needle", "ignored": "needle"})
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle"})
	if err != nil || !strings.Contains(result.Content, "keep") || strings.Contains(result.Content, "ignored") {
		t.Fatalf("result = %q, %v", result.Content, err)
	}
}

func TestGrepIgnoreRuleWithLeadingSpaceIsNotAComment(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{".gitignore": " #literal\n", " #literal": "needle"})
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle"})
	if err != nil || result.Content != "No files found" {
		t.Fatalf("result = %q, %v", result.Content, err)
	}
}

func TestGrepBytePaginationHasNoGaps(t *testing.T) {
	var body strings.Builder
	for i := range 150 {
		fmt.Fprintf(&body, "needle %03d %s\n", i, strings.Repeat("x", 400))
	}
	root := grepReviewRoot(t, map[string]string{"file": body.String()})
	skip := 0
	for range 10 {
		result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle", "output_mode": "content", "head_limit": 0, "skip": skip})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Content) > tool.MaxGrepResultBytes {
			t.Fatalf("output exceeds byte budget: %d", len(result.Content))
		}
		count := 0
		for _, line := range strings.Split(result.Content, "\n") {
			if strings.HasPrefix(line, "file:") {
				want := fmt.Sprintf("file:%d:needle %03d ", skip+count+1, skip+count)
				if !strings.HasPrefix(line, want) {
					t.Fatalf("pagination gap: %q, want %q", line, want)
				}
				count++
			}
		}
		if count == 0 {
			t.Fatal("pagination made no progress")
		}
		skip += count
		if skip == 150 {
			return
		}
		if !strings.Contains(result.Content, fmt.Sprintf("continue with skip: %d", skip)) {
			t.Fatalf("missing next page: %s", result.Content)
		}
	}
	t.Fatalf("did not retrieve all entries: %d", skip)
}

func TestGrepContextPaginationMatchesFullOutput(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": "zero\nfirst needle\ntwo\nthree\nfour\nfive\nsecond needle\nlast\n"})
	args := map[string]any{"pattern": "needle", "output_mode": "content", "-C": 1, "head_limit": 0}
	full, err := GrepTool(root).Execute(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	var collected []string
	args["head_limit"] = 2
	for skip := 0; skip < 20; {
		args["skip"] = skip
		result, err := GrepTool(root).Execute(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		if result.Content == "No matches found" {
			break
		}
		body, _, _ := strings.Cut(result.Content, "\n\n[")
		entries := strings.Split(body, "\n")
		if len(entries) > 2 {
			t.Fatalf("context exceeded entry limit: %q", body)
		}
		collected = append(collected, entries...)
		skip += len(entries)
	}
	if got := strings.Join(collected, "\n"); got != full.Content {
		t.Fatalf("pagination = %q, want %q", got, full.Content)
	}
}

func TestGrepMultilinePreservesInput(t *testing.T) {
	for _, tc := range []struct{ name, data, pattern string }{
		{"final newline", "needle\n", `needle\n\z`},
		{"CRLF", "needle\r\nnext", `needle\r\nnext`},
		{"empty file", "", `^$`},
	} {
		for _, mode := range []string{"files_with_matches", "count", "content"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("grep panicked: %v", p)
					}
				}()
				root := grepReviewRoot(t, map[string]string{"file": tc.data})
				result, err := GrepTool(root).Execute(t.Context(), map[string]any{"path": "file", "pattern": tc.pattern, "multiline": true, "output_mode": mode})
				if err != nil || !strings.Contains(result.Content, "file") || strings.HasPrefix(result.Content, "No ") {
					t.Fatalf("result = %q, %v", result.Content, err)
				}
			})
		}
	}
}

func TestGrepCancelledSingleFile(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": "needle\n"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, mode := range []string{"files_with_matches", "count", "content"} {
		_, err := GrepTool(root).Execute(ctx, map[string]any{"path": "file", "pattern": "needle", "output_mode": mode})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: error = %v", mode, err)
		}
	}
}

func TestGrepDoesNotHideReadFailures(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": "needle\n"})
	if err := root.Chmod("file", 0); err != nil {
		t.Fatal(err)
	}
	defer root.Chmod("file", 0644)
	if _, err := root.ReadFile("file"); err == nil {
		t.Skip("file permissions are not enforced")
	}
	for _, path := range []string{"file", "."} {
		for _, mode := range []string{"files_with_matches", "count", "content"} {
			_, err := GrepTool(root).Execute(t.Context(), map[string]any{"path": path, "pattern": "needle", "output_mode": mode})
			if !errors.Is(err, os.ErrPermission) {
				t.Errorf("%s/%s: error = %v", path, mode, err)
			}
		}
	}
}

func TestGrepDoesNotReportPartialCountsAsComplete(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": "needle\n" + strings.Repeat("x", MaxScanBufSize+1) + "\nneedle\n"})
	_, err := GrepTool(root).Execute(t.Context(), map[string]any{"path": "file", "pattern": "needle", "output_mode": "count"})
	if err == nil || !strings.Contains(err.Error(), "scan limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestGrepLargeNumericOptionsDoNotOverflow(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"a": "before\nneedle\nafter\n", "b": "needle\n"})
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"content limit", map[string]any{"output_mode": "content", "head_limit": math.MaxInt}},
		{"count pagination", map[string]any{"output_mode": "count", "head_limit": math.MaxInt, "skip": 1}},
		{"files pagination", map[string]any{"head_limit": math.MaxInt, "skip": 1}},
		{"after context", map[string]any{"path": "a", "output_mode": "content", "-A": math.MaxInt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("grep panicked: %v", p)
				}
			}()
			tc.args["pattern"] = "needle"
			result, err := GrepTool(root).Execute(t.Context(), tc.args)
			if err != nil || result.Content == "" || strings.HasPrefix(result.Content, "No ") {
				t.Fatalf("result = %q, %v", result.Content, err)
			}
		})
	}
}

func TestGrepTruncatesUTF8AtCharacterBoundaries(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": "needle" + strings.Repeat("x", 490) + "é" + strings.Repeat("z", 20)})
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle", "output_mode": "content"})
	if err != nil || !utf8.ValidString(result.Content) {
		t.Fatalf("invalid UTF-8 output: %q, %v", result.Content, err)
	}
}

func TestGrepSniffsTheFullBinaryWindow(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"file": strings.Repeat("x", 6000) + "\x00needle"})
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle"})
	if err != nil || result.Content != "No files found" {
		t.Fatalf("binary search = %q, %v", result.Content, err)
	}
}

func TestGrepCombinesBraceAndCommaGlobs(t *testing.T) {
	root := grepReviewRoot(t, map[string]string{"a.go": "needle", "b.ts": "needle", "c.md": "needle", "d.txt": "needle"})
	result, err := GrepTool(root).Execute(t.Context(), map[string]any{"pattern": "needle", "glob": "*.{go,ts},*.md"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.go", "b.ts", "c.md"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("missing %s in %q", want, result.Content)
		}
	}
	if strings.Contains(result.Content, "d.txt") {
		t.Fatalf("unexpected match: %q", result.Content)
	}
}
