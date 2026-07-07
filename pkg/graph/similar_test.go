package graph

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNameTokens(t *testing.T) {
	got := nameTokens("HTTPServerConfig")
	want := map[string]bool{"http": true, "server": true, "config": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nameTokens = %v, want %v", got, want)
	}
	if g := nameTokens("parse_config"); !g["parse"] || !g["config"] {
		t.Fatalf("snake_case tokens = %v", g)
	}
}

func TestFindSimilar(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", `package p

func helperX() {}
func helperY() {}
func Alpha() { helperX(); helperY() }
func Beta()  { helperX(); helperY() }
func Gamma() { println("x") }
`)
	e := New(root, filepath.Join(t.TempDir(), "graph.json"))
	ctx := context.Background()
	if _, err := e.Index(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := e.Similar(ctx, "Alpha", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) == 0 || res.Matches[0].Node.Name != "Beta" {
		t.Fatalf("top match for Alpha = %+v, want Beta", res.Matches)
	}
	for _, m := range res.Matches {
		if m.Node.Name == "Gamma" {
			t.Fatal("Gamma shares nothing with Alpha, should not match")
		}
	}
}
