package treesitter

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func newWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()

	writeFile(t, root, "main.go", `package main

func main() {
	greet("world")
}
`)
	writeFile(t, root, "lib/greet.go", `package lib

func greet(name string) string {
	return "hi " + name
}
`)
	writeFile(t, root, "web/app.ts", `export class Animal {
	speak(): string {
		return makeNoise();
	}
}

export class Dog extends Animal {}

function makeNoise(): string {
	return "woof";
}

export function greet(): string {
	return "hello";
}
`)
	writeFile(t, root, "web/view.tsx", `import { Animal } from "./app";

export class Cat extends Animal {}
`)
	return NewWorkspace(root)
}

func locationOf(d Definition) Location {
	return Location{File: d.File, Line: d.NameRange.StartLine, Col: d.NameRange.StartCol}
}

func locationsOf(defs []Definition) []Location {
	out := make([]Location, 0, len(defs))
	for _, d := range defs {
		out = append(out, locationOf(d))
	}
	return out
}

func TestDefinitionsFromCallSite(t *testing.T) {
	w := newWorkspace(t)

	defs, err := w.Definitions(t.Context(), "main.go", nil, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("definitions = %+v, want only the Go definition despite the TS greet", defs)
	}
	if loc := locationOf(defs[0]); loc != (Location{File: "lib/greet.go", Line: 2, Col: 5}) {
		t.Fatalf("location = %+v, want lib/greet.go:2:5", loc)
	}
}

func TestDefinitionsAtEndOfWord(t *testing.T) {
	w := newWorkspace(t)

	defs, err := w.Definitions(t.Context(), "main.go", nil, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].File != "lib/greet.go" {
		t.Fatalf("definitions = %+v, want lib/greet.go", defs)
	}
}

func TestDefinitionsAcrossLangFamilyPreferImports(t *testing.T) {
	w := newWorkspace(t)
	writeFile(t, w.root, "other/zoo.ts", "export class Animal {}\n")

	defs, err := w.Definitions(t.Context(), "web/view.tsx", nil, 2, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].File != "web/app.ts" || defs[0].NameRange.StartLine != 0 {
		t.Fatalf("definitions = %+v, want imported web/app.ts Animal first", locationsOf(defs))
	}
}

func TestDefinitionsPreferSameFile(t *testing.T) {
	w := newWorkspace(t)

	defs, err := w.Definitions(t.Context(), "web/app.ts", nil, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 || defs[0].File != "web/app.ts" {
		t.Fatalf("definitions = %+v, want web/app.ts first", defs)
	}
}

func TestDefinitionsUseProvidedContent(t *testing.T) {
	w := newWorkspace(t)

	edited := []byte("package main\n\nfunc main() {\n\tx := greet\n\t_ = x\n}\n")
	defs, err := w.Definitions(t.Context(), "main.go", edited, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].File != "lib/greet.go" {
		t.Fatalf("definitions = %+v, want lib/greet.go", defs)
	}

	unsaved := []byte("package main\n\nfunc helper() {}\n\nfunc main() {\n\thelper()\n}\n")
	defs, err = w.Definitions(t.Context(), "main.go", unsaved, 5, 2)
	if err != nil || len(defs) != 1 || defs[0].NameRange.StartLine != 2 {
		t.Fatalf("definitions = %+v, err = %v, want helper from the unsaved buffer", defs, err)
	}
}

func TestDefinitionsNoIdentifier(t *testing.T) {
	w := newWorkspace(t)

	defs, err := w.Definitions(t.Context(), "main.go", nil, 0, 0)
	if err != nil || len(defs) != 0 {
		t.Fatalf("definitions = %+v, err = %v, want none on keyword", defs, err)
	}
}

func TestQualifierPinsImportedPackage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "alpha/alpha.go", "package alpha\n\nfunc New() {}\n")
	writeFile(t, root, "beta/beta.go", "package beta\n\nfunc New() {}\n")
	writeFile(t, root, "main.go", "package main\n\nimport (\n\t\"example.com/m/alpha\"\n\t\"example.com/m/beta\"\n)\n\nfunc main() {\n\talpha.New()\n\tbeta.New()\n}\n")
	w := NewWorkspace(root)

	defs, err := w.Definitions(t.Context(), "main.go", nil, 9, 7)
	if err != nil || len(defs) != 2 || defs[0].File != "beta/beta.go" {
		t.Fatalf("definitions = %+v, err = %v, want beta.New first", locationsOf(defs), err)
	}
	calls, err := w.OutgoingCalls(t.Context(), "main.go", nil, 7, 6)
	if err != nil || len(calls) != 1 || calls[0].File != "alpha/alpha.go" {
		t.Fatalf("callees = %+v, err = %v, want the first site's alpha.New", calls, err)
	}
}

func TestGoUnexportedNamesStayInPackage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/a.go", "package a\n\nfunc helper() {}\n\nfunc Run() { helper() }\n")
	writeFile(t, root, "b/b.go", "package b\n\nfunc helper() {}\n\nfunc Run() { helper() }\n")
	w := NewWorkspace(root)

	defs, err := w.Definitions(t.Context(), "a/a.go", nil, 4, 14)
	if err != nil || len(defs) != 1 || defs[0].File != "a/a.go" {
		t.Fatalf("definitions = %+v, err = %v, want only a/a.go", locationsOf(defs), err)
	}
	refs, _, err := w.References(t.Context(), "a/a.go", nil, 2, 6, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.File != "a/a.go" {
			t.Fatalf("references = %+v, want package a only", refs)
		}
	}
	if len(refs) != 2 {
		t.Fatalf("references = %+v, want declaration and call", refs)
	}
}

func TestReferencesUseIdentifiersNotText(t *testing.T) {
	w := newWorkspace(t)
	writeFile(t, w.root, "lib/use.go", "package lib\n\n// greet is documented here.\nvar message = \"greet\"\nvar fn = greet\n")

	refs, truncated, err := w.References(t.Context(), "lib/greet.go", nil, 2, 6, true)
	if err != nil || truncated {
		t.Fatalf("references err = %v truncated = %v", err, truncated)
	}
	want := []Location{{"lib/greet.go", 2, 5}, {"lib/use.go", 4, 9}}
	if !slices.Equal(refs, want) {
		t.Fatalf("references = %+v, want %+v (comments, strings and other packages excluded)", refs, want)
	}

	uses, _, err := w.References(t.Context(), "lib/greet.go", nil, 2, 6, false)
	if err != nil || !slices.Equal(uses, want[1:]) {
		t.Fatalf("uses = %+v, err = %v, want declaration excluded", uses, err)
	}
}

func TestReferencesFindHierarchySites(t *testing.T) {
	w := newWorkspace(t)

	refs, _, err := w.References(t.Context(), "web/app.ts", nil, 0, 14, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []Location{{"web/app.ts", 0, 13}, {"web/app.ts", 6, 25}, {"web/view.tsx", 0, 9}, {"web/view.tsx", 2, 25}} {
		if !slices.Contains(refs, want) {
			t.Fatalf("references = %+v, missing %+v", refs, want)
		}
	}
}

func TestHoverShowsDefinitionSnippet(t *testing.T) {
	w := newWorkspace(t)

	info, err := w.Hover(t.Context(), "main.go", nil, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.Definition.File != "lib/greet.go" {
		t.Fatalf("info = %+v, want lib/greet.go", info)
	}
	if !strings.Contains(info.Code, "func greet(name string) string") || info.Truncated {
		t.Fatalf("code = %q, want full short signature", info.Code)
	}
	if info.Others != 0 {
		t.Fatalf("others = %d, want 0 (TS greet is another family)", info.Others)
	}

	if info, err := w.Hover(t.Context(), "main.go", nil, 0, 0); err != nil || info != nil {
		t.Fatalf("info = %+v, err = %v, want nil on keyword", info, err)
	}
}

func TestImplementationsFindSubtypes(t *testing.T) {
	w := newWorkspace(t)

	defs, err := w.Implementations(t.Context(), "web/app.ts", nil, 0, 14)
	if err != nil {
		t.Fatal(err)
	}
	want := []Location{{"web/app.ts", 6, 13}, {"web/view.tsx", 2, 13}}
	if got := locationsOf(defs); !slices.Equal(got, want) {
		t.Fatalf("implementations = %+v, want Dog and Cat %+v", got, want)
	}

	if defs, err := w.Implementations(t.Context(), "lib/greet.go", nil, 2, 6); err != nil || len(defs) != 0 {
		t.Fatalf("implementations = %+v, err = %v, want none for functions", defs, err)
	}
}

func TestImplementationsAcrossLanguages(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "shapes.ts", "interface Shape { area(): number }\nclass Base {}\nclass Circle extends Base implements Shape {\n  area() { return 1 }\n}\n")
	writeFile(t, root, "animals.py", "class Animal:\n    pass\n\nclass Dog(Animal):\n    pass\n")
	writeFile(t, root, "types.go", "package main\n\ntype Base struct{ x int }\n\ntype Derived struct {\n\tBase\n\ty int\n}\n")
	w := NewWorkspace(root)

	for _, tc := range []struct {
		file      string
		line, col int
		want      string
	}{
		{"shapes.ts", 0, 11, "Circle"},
		{"shapes.ts", 1, 7, "Circle"},
		{"animals.py", 0, 7, "Dog"},
		{"types.go", 2, 6, "Derived"},
	} {
		defs, err := w.Implementations(t.Context(), tc.file, nil, tc.line, tc.col)
		if err != nil || len(defs) != 1 || defs[0].Name != tc.want {
			t.Errorf("%s:%d implementations = %+v, err = %v, want %s", tc.file, tc.line, defs, err, tc.want)
		}
	}
}

func TestIncomingCallsGroupByCaller(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", `package app

func leaf() {}

func a() {
	leaf()
	leaf()
}

func b() { leaf() }

var _ = func() int { leaf(); return 0 }()
`)
	w := NewWorkspace(root)

	calls, err := w.IncomingCalls(t.Context(), "app.go", nil, 2, 6)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, call := range calls {
		got = append(got, call.Name)
		if call.Name == "a" && len(call.Sites) != 2 {
			t.Fatalf("a sites = %+v, want two call sites", call.Sites)
		}
	}
	if !slices.Contains(got, "a") || !slices.Contains(got, "b") || len(calls) != 3 {
		t.Fatalf("callers = %v, want a, b and the top-level initializer", got)
	}
}

func TestIncomingCallsKeepTopLevelCallsApartFromFirstDeclaration(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.py", "def leaf():\n    leaf()\n\nleaf()\n")
	w := NewWorkspace(root)

	calls, err := w.IncomingCalls(t.Context(), "app.py", nil, 0, 5)
	if err != nil || len(calls) != 2 {
		t.Fatalf("callers = %+v, err = %v, want leaf itself and the module", calls, err)
	}
	for _, call := range calls {
		if len(call.Sites) != 1 {
			t.Fatalf("callers = %+v, want one site each", calls)
		}
	}
}

func TestOutgoingCallsResolveCallees(t *testing.T) {
	w := newWorkspace(t)
	writeFile(t, w.root, "web/util.ts", "export function external() {}\nexport function run() {\n\tmakeNoise();\n\tnew Animal().speak();\n\tconsole.log(missing());\n}\n")

	calls, err := w.OutgoingCalls(t.Context(), "web/util.ts", nil, 1, 17)
	if err != nil {
		t.Fatal(err)
	}
	resolved := map[string]string{}
	for _, call := range calls {
		resolved[call.Name] = call.File
	}
	if resolved["makeNoise"] != "web/app.ts" || resolved["speak"] != "web/app.ts" {
		t.Fatalf("callees = %+v, want makeNoise and speak in web/app.ts", calls)
	}
	if file, ok := resolved["missing"]; !ok || file != "" {
		t.Fatalf("callees = %+v, want unresolved missing", calls)
	}
	if last := calls[len(calls)-1]; last.File != "" {
		t.Fatalf("callees = %+v, want unresolved callees last", calls)
	}

	calls, err = w.OutgoingCalls(t.Context(), "web/app.ts", nil, 1, 2)
	if err != nil || len(calls) != 1 || calls[0].Name != "makeNoise" || calls[0].Sites[0].StartLine != 2 {
		t.Fatalf("speak callees = %+v, err = %v, want makeNoise at line 2", calls, err)
	}
}

func TestOutgoingCallsRejectAmbiguousCallSite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/run.ts", "export function run() {}\n")
	writeFile(t, root, "b/run.ts", "export function run() {}\n")
	writeFile(t, root, "c/main.ts", "run();\n")
	w := NewWorkspace(root)

	if _, err := w.OutgoingCalls(t.Context(), "c/main.ts", nil, 0, 1); err == nil || !strings.Contains(err.Error(), "a/run.ts:1") {
		t.Fatalf("err = %v, want ambiguity listing both declarations", err)
	}
}

func TestOutgoingCallsExcludeNestedDeclarations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.ts", `function leaf() {}
function outer() {
  function inner() { leaf(); }
  inner();
}
`)
	w := NewWorkspace(root)
	for _, tc := range []struct {
		line, col int
		want      string
	}{{1, 10, "inner"}, {2, 12, "leaf"}} {
		calls, err := w.OutgoingCalls(t.Context(), "app.ts", nil, tc.line, tc.col)
		if err != nil || len(calls) != 1 || calls[0].Name != tc.want {
			t.Fatalf("calls at %d:%d = %+v, err = %v, want only %s", tc.line, tc.col, calls, err, tc.want)
		}
	}
}

func TestOutgoingCallsUseProvidedContent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package app\nfunc run() {}\n")
	source := []byte("package app\nfunc run() { helper() }\nfunc helper() {}\n")
	calls, err := NewWorkspace(root).OutgoingCalls(t.Context(), "app.go", source, 1, 6)
	if err != nil || len(calls) != 1 || calls[0].File != "app.go" || calls[0].NameRange.StartLine != 2 {
		t.Fatalf("calls = %+v, err = %v, want helper from the unsaved buffer", calls, err)
	}
}

func TestSymbolsRankAndLimit(t *testing.T) {
	w := newWorkspace(t)
	writeFile(t, w.root, "lib/greeter.go", "package lib\n\ntype Greeter struct{}\n\nfunc newGreeter() *Greeter { return nil }\n")

	defs, truncated, err := w.Symbols(t.Context(), "greet", 10, nil)
	if err != nil || truncated {
		t.Fatalf("symbols err = %v truncated = %v", err, truncated)
	}
	var names []string
	for _, d := range defs {
		names = append(names, d.Name+"@"+d.File)
	}
	if len(defs) != 4 || defs[0].Name != "greet" || defs[1].Name != "greet" || defs[2].Name != "Greeter" {
		t.Fatalf("symbols = %v, want exact, then prefix, then contains matches", names)
	}

	defs, truncated, err = w.Symbols(t.Context(), "greet", 1, nil)
	if err != nil || !truncated || len(defs) != 1 || defs[0].Name != "greet" {
		t.Fatalf("symbols = %+v truncated = %v err = %v, want the exact match kept by the limit", defs, truncated, err)
	}
	if defs, truncated, err = w.Symbols(t.Context(), "", 0, nil); err != nil || truncated || len(defs) < 8 {
		t.Fatalf("symbols = %d truncated = %v err = %v, want every declaration without a limit", len(defs), truncated, err)
	}

	defs, truncated, err = w.Symbols(t.Context(), "", 2, nil)
	if err != nil || !truncated || len(defs) != 2 {
		t.Fatalf("symbols = %+v truncated = %v err = %v, want two of many", defs, truncated, err)
	}

	defs, _, err = w.Symbols(t.Context(), "greet", 10, func(file string) bool { return strings.HasPrefix(file, "web/") })
	if err != nil || len(defs) != 1 || defs[0].File != "web/app.ts" {
		t.Fatalf("symbols = %+v, err = %v, want only included files", defs, err)
	}
}

func TestScanSkipsGeneratedAndHiddenDirectories(t *testing.T) {
	w := newWorkspace(t)
	writeFile(t, w.root, "node_modules/pkg/index.ts", "export class Animal {}\n")
	writeFile(t, w.root, ".cache/app.ts", "export class Animal {}\n")
	writeFile(t, w.root, "web/bundle.min.js", "class Animal {}"+strings.Repeat(" ", 3000)+"\n")

	defs, err := w.Definitions(t.Context(), "web/view.tsx", nil, 2, 25)
	if err != nil || len(defs) != 1 {
		t.Fatalf("definitions = %+v, err = %v, want only web/app.ts", locationsOf(defs), err)
	}
}

func TestScanLinkedWorkspaceRoot(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			target := t.TempDir()
			writeFile(t, target, "project/main.go", "package main\nfunc main() {}")
			root := filepath.Join(t.TempDir(), "workspace")
			testenv.DirLink(t, kind, target, root)
			outside := t.TempDir()
			writeFile(t, outside, "secret.go", "package main\nfunc main() {}")
			testenv.DirLink(t, kind, outside, filepath.Join(target, "escape"))
			testenv.DirLink(t, kind, target, filepath.Join(target, "loop"))

			defs, _, err := NewWorkspace(root).Symbols(t.Context(), "main", 10, nil)
			if err != nil || len(defs) != 1 || defs[0].File != "project/main.go" {
				t.Fatalf("symbols = %+v, err = %v, want only project/main.go", defs, err)
			}
		})
	}
}

func TestQueriesRespectCancellation(t *testing.T) {
	w := newWorkspace(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := w.Symbols(ctx, "", 10, nil); err == nil {
		t.Fatal("want cancellation error")
	}
}
