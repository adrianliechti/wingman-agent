package graph

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestPaginationDoesNotOverflow(t *testing.T) {
	e := newContentTestEngine(t)
	page, err := e.SearchPage(t.Context(), SearchOpts{Offset: 1, Limit: math.MaxInt})
	if err != nil || page.Offset != 1 || len(page.Nodes) != page.Total-1 || page.HasMore {
		t.Fatalf("search page: %+v %v", page, err)
	}
	content, err := e.SearchContent(t.Context(), ContentSearchOpts{Pattern: "NEEDLE", Offset: 1, Limit: math.MaxInt})
	if err != nil || content.Offset != 1 || len(content.Hits) != content.TotalResults-1 || content.HasMore {
		t.Fatalf("content page: %+v %v", content, err)
	}
}

func TestChangedSourceDoesNotUseCachedDefinitionRanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "source.go", "package sample\nfunc Before() { println(\"needle\") }\n")
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	if _, err := e.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "source.go", "package sample\nfunc DifferentFunction() { println(\"needle\") }\n")
	if _, err := e.Snippet(t.Context(), "Before", ""); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("snippet used stale ranges: %v", err)
	}
	result, err := e.SearchContent(t.Context(), ContentSearchOpts{Pattern: "needle"})
	if err != nil || !result.SourceChanged || len(result.Hits) != 0 || len(result.Raw) != 1 || !strings.Contains(result.Raw[0].Content, "DifferentFunction") {
		t.Fatalf("content attributed to stale definition: %+v %v", result, err)
	}
	if _, err := e.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	snippet, err := e.Snippet(t.Context(), "DifferentFunction", "")
	if err != nil || !strings.Contains(snippet.Code, "DifferentFunction") {
		t.Fatalf("fresh snippet: %+v %v", snippet, err)
	}
}

func TestContentSearchReportsIncompleteScans(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "long.go", "//"+strings.Repeat("x", maxContentLineBytes)+"\n// needle\n")
	e := New(root, "")
	result, err := e.SearchContent(t.Context(), ContentSearchOpts{Pattern: "needle"})
	if err != nil || len(result.SkippedFiles) != 1 || !strings.Contains(result.SkippedFiles[0].Reason, "scan incomplete") {
		t.Fatalf("long-line truncation hidden: %+v %v", result, err)
	}
}

func TestGraphQueriesRespectCancellationWhileWaitingForIndex(t *testing.T) {
	e := New(t.TempDir(), "")
	if err := e.buildGate.Acquire(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	defer e.buildGate.Release(1)
	for _, query := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := e.Index(ctx); return err },
		func(ctx context.Context) error { _, err := e.SearchPage(ctx, SearchOpts{}); return err },
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- query(ctx) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("waiting query: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("cancelled query remained blocked on index")
		}
		cancel()
	}
}

func TestCachedSearchAndHistoryRespectCancelledContext(t *testing.T) {
	e := newContentTestEngine(t)
	if _, err := e.Index(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.SearchPage(ctx, SearchOpts{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached search: %v", err)
	}
	if _, err := e.CoChanges(ctx, "app.go", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("history query: %v", err)
	}
}

func TestLeadingDotSlashFileFilterAndCRLFSnippet(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/a.go", "package pkg\r\nfunc Alpha() {\r\n\tBeta()\r\n}\r\nfunc Beta() {}\r\n")
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))

	search, err := e.SearchPage(t.Context(), SearchOpts{Query: "Alpha", File: "./pkg/a.go"})
	if err != nil || len(search.Nodes) != 1 {
		t.Fatalf("SearchPage with ./pkg/a.go: %+v err=%v", search, err)
	}
	content, err := e.SearchContent(t.Context(), ContentSearchOpts{Pattern: "Beta", File: "./pkg/a.go"})
	if err != nil || len(content.Hits) != 2 {
		t.Fatalf("SearchContent with ./pkg/a.go: %+v err=%v", content, err)
	}
	snippet, err := e.Snippet(t.Context(), "Alpha", "./pkg/a.go")
	if err != nil || strings.Contains(snippet.Code, "\r") || !strings.Contains(snippet.Code, "func Alpha()") {
		t.Fatalf("Snippet with ./pkg/a.go and CRLF: %q err=%v", snippet.Code, err)
	}
}

func TestGitChangesSkipsNonRegularReplacedPath(t *testing.T) {
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "app.go", "package p\nfunc A() {}\n")
	if _, err := wt.Add("app.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("c1", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "app.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "app.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	if _, err := e.DetectChanges(t.Context(), ""); err != nil {
		t.Fatalf("DetectChanges failed when file replaced by directory: %v", err)
	}
}
