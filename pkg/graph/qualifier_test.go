package graph

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestQualifiedSymbolErrorOffersExistingDefinitions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "engine.go", "package sample\ntype Engine struct{}\nfunc (e *Engine) ServeHTTP() { Handle() }\nfunc Handle() {}\n")
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	for _, query := range []string{"Engine.ServeHTTP", "WrongType.ServeHTTP"} {
		_, err := e.Trace(t.Context(), query, "Handle", "", false, 8)
		if err == nil || !strings.Contains(err.Error(), "engine.go:ServeHTTP (line 3)") || !strings.Contains(err.Error(), "verify receiver/class") {
			t.Fatalf("qualified query must retain its error and offer a verified definition: %v", err)
		}
	}
	result, err := e.Trace(t.Context(), "engine.go:ServeHTTP", "Handle", "", false, 8)
	if err != nil || len(result.Paths) != 1 {
		t.Fatalf("suggested syntax must work: %+v, %v", result, err)
	}
	_, err = e.Trace(t.Context(), "Engine.ServeHTTP", "", "other/", false, 8)
	if err == nil || strings.Contains(err.Error(), "engine.go:ServeHTTP") {
		t.Fatalf("suggestions must respect explicit file filter: %v", err)
	}
}

func TestQualifiedSymbolSuggestionsAreBoundedAndDoNotInventMatches(t *testing.T) {
	root := t.TempDir()
	for i := range 8 {
		writeFile(t, root, fmt.Sprintf("p%d/run.go", i), "package sample\nfunc Run() {}\n")
	}
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	_, err := e.Trace(t.Context(), "Unknown.Run", "", "", false, 8)
	if err == nil {
		t.Fatal("unknown qualifier silently accepted")
	}
	message := err.Error()
	if strings.Count(message, "\n- ") != 5 || !strings.Contains(message, "showing 5 of 8") {
		t.Fatal(message)
	}
	_, err = e.Trace(t.Context(), "Unknown.Missing", "", "", false, 8)
	if err == nil || strings.Contains(err.Error(), "definitions") {
		t.Fatal("invented candidates:", err)
	}
}
