package codeintel

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type fixture struct {
	name    string
	request string
	files   map[string]string
	answer  any
	after   map[string]string
}

type referenceAnswer struct {
	Definition string   `json:"definition"`
	Callers    []string `json:"callers"`
}
type pathAnswer struct {
	Paths [][]string `json:"paths"`
}
type countsAnswer struct {
	Counts map[string]int `json:"counts"`
}

func fixtures() []fixture {
	refs := fixture{name: "receiver_references", request: "Find the declaration and every direct call site of Beta.Close, including calls through variables with an alias of Beta. Exclude Alpha.Close, comments and strings. Return JSON {\"definition\":\"file:line\",\"callers\":[\"file:line\",...]}. Cite each call's line, not its enclosing declaration.", files: map[string]string{
		"types.go": "package sample\n\ntype Alpha struct{}\nfunc (Alpha) Close() {}\ntype Beta struct{}\nfunc (Beta) Close() {}\ntype Alias = Beta\n",
		"uses.go":  "package sample\n\nfunc First() { var b Beta; b.Close() }\nfunc Second(b *Beta) { b.Close() }\nfunc Unrelated(a Alpha) { a.Close() }\n// Beta.Close appears in documentation.\nvar Text = \"Beta.Close\"\nfunc Third(b Alias) { b.Close() }\n",
		"more.go":  "package sample\n\nfunc Fourth(b Beta) { alias := b; alias.Close() }\nfunc Fifth(a Alpha) { a.Close() }\n",
	}, answer: referenceAnswer{Definition: "types.go:6", Callers: []string{"more.go:3", "uses.go:3", "uses.go:4", "uses.go:8"}}}
	paths := fixture{name: "multi_hop", request: "Find every simple direct-call path from Handle to WriteRecord in this repository. Exclude comments, strings and paths starting at other entry points. Return JSON {\"paths\":[[\"Function\",...],...]}, each path including Handle and WriteRecord, using bare function names.", files: map[string]string{
		"entry.go":   "package sample\n\nfunc Handle() { Validate(); Process(); Audit() }\nfunc OtherEntry() { WriteRecord() }\n// Handle -> WriteRecord is a comment, not an edge.\nvar PathText = \"Handle WriteRecord\"\n",
		"service.go": "package sample\n\nfunc Process() { Normalize(); Persist() }\nfunc Validate() { CheckPayload() }\nfunc Normalize() {}\nfunc CheckPayload() {}\n",
		"storage.go": "package sample\n\nfunc Persist() { Save() }\nfunc Save() { WriteRecord() }\nfunc WriteRecord() {}\n",
		"audit.go":   "package sample\n\nfunc Audit() { AppendAudit() }\nfunc AppendAudit() { WriteRecord() }\n",
	}, answer: pathAnswer{Paths: [][]string{{"Handle", "Audit", "AppendAudit", "WriteRecord"}, {"Handle", "Process", "Persist", "Save", "WriteRecord"}}}}
	counts := fixture{name: "reference_completeness", request: "Count every direct call to the package function Target, grouped by source file. Exclude its declaration, comments, strings, and calls to locally shadowed Target variables. Return JSON {\"counts\":{\"file.go\":number,...}}, containing only files with real call sites. Check completeness across the whole repository.", files: map[string]string{
		"target.go": "package sample\n\nfunc Target() int { return 42 }\n",
		"decoy.go":  "package sample\n\n// Target() is mentioned here.\nvar Text = \"Target()\"\nfunc Shadow() int { Target := func() int { return 7 }; return Target() }\n",
	}, answer: countsAnswer{Counts: map[string]int{}}}
	for group := 0; group < 5; group++ {
		name := fmt.Sprintf("calls_%d.go", group)
		counts.files[name] = "package sample\n\n"
		count := 23 + group // 125 total, different per-file counts defeat symmetry guesses.
		for i := 0; i < count; i++ {
			counts.files[name] += fmt.Sprintf("func Use%d_%02d() int { return Target() }\n", group, i)
		}
		counts.answer.(countsAnswer).Counts[name] = count
	}
	rename := fixture{name: "semantic_rename", request: "Rename the package function Target declared in target.go to Renamed and update every direct call across the repository. Preserve all comments, strings, and unrelated local variables with the same name. Apply the changes using the available tools. Finish with a brief summary.", files: map[string]string{
		"target.go": "package sample\n\nfunc Target() int { return 42 }\n",
		"uses.go":   "package sample\n\nfunc First() int { return Target() }\nfunc Second() int { return Target() }\n",
		"more.go":   "package sample\n\nfunc Third() int { return Target() + 1 }\n",
		"decoy.go":  "package sample\n\n// Target remains in this comment.\nvar Text = \"Target\"\nfunc Shadow() int { Target := 7; return Target }\nfunc ShadowCall() int { Target := func() int { return 9 }; return Target() }\n",
	}, after: map[string]string{}}
	for name, before := range rename.files {
		after := before
		if name != "decoy.go" {
			after = strings.ReplaceAll(before, "Target", "Renamed")
		}
		rename.after[name] = after
	}
	all := []fixture{refs, paths, counts, rename, largeCallGraph()}
	for i := range all {
		all[i].files["go.mod"] = "module codeintel-fixture.test\n\ngo 1.26\n"
		for n := 0; n < 8; n++ {
			name := fmt.Sprintf("noise_%02d.go", n)
			all[i].files[name] = fmt.Sprintf("package sample\n\nfunc Unrelated%02d() int { return %d }\n", n, n)
		}
		if all[i].after != nil {
			for name, before := range all[i].files {
				if _, ok := all[i].after[name]; !ok {
					all[i].after[name] = before
				}
			}
		}
	}
	return all
}

func writeFixture(t *testing.T, f fixture) string {
	t.Helper()
	root, err := os.MkdirTemp("", "codeintel-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	for name, data := range f.files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func score(f fixture, root, final string) (bool, string) {
	if f.after != nil {
		entries, err := os.ReadDir(root)
		if err != nil {
			return false, err.Error()
		}
		if len(entries) != len(f.after) {
			return false, "unexpected added/deleted files"
		}
		for name, want := range f.after {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				return false, err.Error()
			}
			if strings.HasSuffix(name, ".go") {
				if _, err := parser.ParseFile(token.NewFileSet(), name, got, parser.AllErrors); err != nil {
					return false, err.Error()
				}
				if !slices.Equal(tokens(string(got)), tokens(want)) {
					return false, "unexpected source tokens in " + name
				}
			} else if string(got) != want {
				return false, "unexpected change in " + name
			}
		}
		return true, ""
	}
	final = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(final), "```json"), "```"), "```"))
	var got any
	switch f.answer.(type) {
	case referenceAnswer:
		got = &referenceAnswer{}
	case pathAnswer:
		got = &pathAnswer{}
	case countsAnswer:
		got = &countsAnswer{}
	}
	decoder := json.NewDecoder(strings.NewReader(final))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(got); err != nil {
		return false, "invalid answer JSON: " + err.Error()
	}
	if decoder.Decode(new(any)) != io.EOF {
		return false, "answer contains trailing data"
	}
	switch answer := got.(type) {
	case *referenceAnswer:
		slices.Sort(answer.Callers)
		want := f.answer.(referenceAnswer)
		if reflect.DeepEqual(*answer, want) {
			return true, ""
		}
	case *pathAnswer:
		slices.SortFunc(answer.Paths, func(a, b []string) int { return strings.Compare(strings.Join(a, "/"), strings.Join(b, "/")) })
		if reflect.DeepEqual(*answer, f.answer) {
			return true, ""
		}
	case *countsAnswer:
		if reflect.DeepEqual(*answer, f.answer) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("answer mismatch: got %s; want %+v", final, f.answer)
}

func tokens(text string) []string {
	var s scanner.Scanner
	s.Init(token.NewFileSet().AddFile("", -1, len(text)), []byte(text), nil, scanner.ScanComments)
	var values []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return values
		}
		if lit == "" {
			lit = tok.String()
		}
		values = append(values, lit)
	}
}

// Ground truth is independently checked with Go's parser/type checker, not with
// either tool being evaluated. Comment/string and receiver decoys are deliberate.
func TestFixtureGroundTruth(t *testing.T) {
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			fset := token.NewFileSet()
			var files []*ast.File
			for name, data := range f.files {
				if strings.HasSuffix(name, ".go") {
					parsed, err := parser.ParseFile(fset, name, data, 0)
					if err != nil {
						t.Fatal(err)
					}
					files = append(files, parsed)
				}
			}
			info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
			pkg, err := new(types.Config).Check("sample", fset, files, info)
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string][]string{}
			adj := map[string][]string{}
			for _, file := range files {
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok {
						continue
					}
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						var id *ast.Ident
						switch callee := call.Fun.(type) {
						case *ast.Ident:
							id = callee
						case *ast.SelectorExpr:
							id = callee.Sel
						default:
							return true
						}
						obj, ok := info.Uses[id].(*types.Func)
						if !ok {
							return true
						}
						name := obj.Name()
						if sig := obj.Type().(*types.Signature); sig.Recv() != nil {
							name = types.TypeString(sig.Recv().Type(), func(*types.Package) string { return "" }) + "." + name
						}
						p := fset.Position(id.Pos())
						calls[name] = append(calls[name], fmt.Sprintf("%s:%d", p.Filename, p.Line))
						adj[fn.Name.Name] = append(adj[fn.Name.Name], name)
						return true
					})
				}
			}
			switch want := f.answer.(type) {
			case referenceAnswer:
				slices.Sort(calls["Beta.Close"])
				if !slices.Equal(calls["Beta.Close"], want.Callers) {
					t.Fatalf("compiler calls=%v", calls["Beta.Close"])
				}
				obj, _, _ := types.LookupFieldOrMethod(pkg.Scope().Lookup("Beta").Type(), true, pkg, "Close")
				p := fset.Position(obj.Pos())
				if fmt.Sprintf("%s:%d", p.Filename, p.Line) != want.Definition {
					t.Fatal(p)
				}
			case countsAnswer:
				counts := map[string]int{}
				for _, loc := range calls["Target"] {
					counts[strings.Split(loc, ":")[0]]++
				}
				if !reflect.DeepEqual(counts, want.Counts) {
					t.Fatal(counts)
				}
			case pathAnswer:
				var paths [][]string
				var walk func(string, []string)
				walk = func(node string, path []string) {
					if slices.Contains(path, node) {
						return
					}
					path = append(slices.Clone(path), node)
					if node == want.Paths[0][len(want.Paths[0])-1] {
						paths = append(paths, path)
						return
					}
					for _, next := range adj[node] {
						walk(next, path)
					}
				}
				walk(want.Paths[0][0], nil)
				slices.SortFunc(paths, func(a, b []string) int { return strings.Compare(strings.Join(a, "/"), strings.Join(b, "/")) })
				if !reflect.DeepEqual(paths, want.Paths) {
					t.Fatal(paths)
				}
			}
			root := writeFixture(t, f)
			if f.after != nil {
				for name, data := range f.after {
					if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			answer, _ := json.Marshal(f.answer)
			if ok, why := score(f, root, string(answer)); !ok {
				t.Fatal(why)
			}
		})
	}
}

func TestScorerRejectsDecoysIncompleteAndDamagingAnswers(t *testing.T) {
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			root := writeFixture(t, f)
			if ok, _ := score(f, root, `{}`); ok {
				t.Fatal("accepted empty answer/unchanged rename")
			}
			var invalid any
			switch want := f.answer.(type) {
			case referenceAnswer:
				invalid = referenceAnswer{Definition: want.Definition, Callers: append(slices.Clone(want.Callers), "uses.go:5")}
			case pathAnswer:
				invalid = pathAnswer{Paths: want.Paths[:1]}
			case countsAnswer:
				invalid = countsAnswer{Counts: map[string]int{"calls_0.go": 125}}
			default:
				for name, data := range f.after {
					if name == "decoy.go" {
						data = strings.ReplaceAll(data, "Target", "Renamed")
					}
					if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			answer, _ := json.Marshal(invalid)
			if ok, _ := score(f, root, string(answer)); ok {
				t.Fatal("accepted decoy, incomplete result, or damaged rename")
			}
		})
	}
}

// A larger structural task prevents the comparison from consisting only of
// tiny files that fit into one grep response. All 127 names are unique; their
// permutation prevents the tree shape being guessed from numeric names.
func largeCallGraph() fixture {
	const count, target, extra = 127, 96, 35
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("Stage%03d", (i*37+11)%count)
	}
	names[0] = "StartWorkflow"
	names[target] = "CommitRecord"
	f := fixture{name: "large_call_graph", request: "Find every simple direct-call path from StartWorkflow to CommitRecord. The repository has many unrelated branches; inspect the actual calls. Return JSON {\"paths\":[[\"Function\",...],...]}, using bare function names and including both endpoints.", files: map[string]string{}}
	for i := range count {
		var calls []string
		for _, child := range []int{2*i + 1, 2*i + 2} {
			if child < count {
				calls = append(calls, names[child]+"()")
			}
		}
		if i == extra {
			calls = append(calls, names[target]+"()")
		}
		f.files[fmt.Sprintf("part_%03d.go", (i*53+7)%count)] = fmt.Sprintf("package sample\n\nfunc %s() { %s }\n", names[i], strings.Join(calls, "; "))
	}
	route := func(leaf int) []string {
		var path []string
		for node := leaf; ; node = (node - 1) / 2 {
			path = append(path, names[node])
			if node == 0 {
				break
			}
		}
		slices.Reverse(path)
		return path
	}
	paths := [][]string{route(target), append(route(extra), names[target])}
	slices.SortFunc(paths, func(a, b []string) int { return strings.Compare(strings.Join(a, "/"), strings.Join(b, "/")) })
	f.answer = pathAnswer{Paths: paths}
	return f
}
