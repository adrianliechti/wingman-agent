package treesitter

import (
	"testing"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

var sampleFiles = map[string]string{
	"go":         "x.go",
	"python":     "x.py",
	"javascript": "x.js",
	"typescript": "x.ts",
	"tsx":        "x.tsx",
}

func TestQueriesCompile(t *testing.T) {
	for kind, queries := range map[string]map[string]string{"import": importQueries, "hierarchy": hierarchyQueries} {
		for lang, query := range queries {
			entry := grammars.DetectLanguage(sampleFiles[lang])
			if entry == nil {
				t.Fatalf("no grammar detected for %s", lang)
			}
			if _, err := gotreesitter.NewQuery(query, entry.Language()); err != nil {
				t.Errorf("%s query for %s failed to compile: %v", kind, lang, err)
			}
		}
	}
}

func TestBuiltinNamesDerivedFromGrammars(t *testing.T) {
	cases := map[string][]string{
		"x.go": {"append", "len", "panic"},
		"x.py": {"len", "print", "isinstance"},
		"x.js": {"require"},
	}
	for file, want := range cases {
		entry := grammars.DetectLanguage(file)
		if entry == nil {
			t.Fatalf("no grammar for %s", file)
		}
		names := builtinNames(entry)
		for _, name := range want {
			if !names[name] {
				t.Errorf("%s builtins missing %q (got %d names)", entry.Name, name, len(names))
			}
		}
	}
}

func TestFileSymbolsNesting(t *testing.T) {
	src := []byte(`export class Animal {
	speak(): string {
		return "";
	}
}

function makeNoise(): string {
	return "woof";
}
`)
	syms := FileSymbols("app.ts", src)
	if len(syms) != 2 {
		t.Fatalf("symbols = %+v, want two roots", syms)
	}
	if syms[0].Name != "Animal" || syms[0].Kind != KindClass {
		t.Fatalf("first = %+v, want class Animal", syms[0])
	}
	if len(syms[0].Children) != 1 || syms[0].Children[0].Name != "speak" {
		t.Fatalf("children = %+v, want speak", syms[0].Children)
	}
	if syms[1].Name != "makeNoise" || syms[1].Kind != KindFunction {
		t.Fatalf("second = %+v, want function makeNoise", syms[1])
	}
	if syms[0].NameRange.StartLine != 0 || syms[0].NameRange.StartCol != 13 {
		t.Fatalf("name range = %+v, want line 0 col 13", syms[0].NameRange)
	}
}

func TestFileSymbolsBoundFunctionsAndGoTypes(t *testing.T) {
	js := FileSymbols("app.tsx", []byte("export const App = () => {\n\treturn null;\n};\nconst value = 1;\n"))
	if len(js) == 0 || js[0].Name != "App" || js[0].Kind != KindFunction {
		t.Fatalf("symbols = %+v, want bound arrow function App", js)
	}
	goSyms := FileSymbols("x.go", []byte("package x\n\ntype Point struct{ X int }\n"))
	if len(goSyms) != 1 || goSyms[0].Name != "Point" || goSyms[0].Kind == "" {
		t.Fatalf("symbols = %+v, want type Point once", goSyms)
	}
}

func TestFileSymbolsUnsupportedFile(t *testing.T) {
	if syms := FileSymbols("readme.txt", []byte("hello")); syms != nil {
		t.Fatalf("symbols = %+v, want nil", syms)
	}
	if Supported("readme.txt") || !Supported("main.go") {
		t.Fatal("Supported must follow grammar tag support")
	}
}

func TestDocumentHighlightsUseIdentifierNodes(t *testing.T) {
	src := []byte("package main\n\nfunc greet() {\n\tgreet()\n\t_ = \"greet\"\n}\n")
	ranges := DocumentHighlights("main.go", src, 3, 3)
	if len(ranges) != 2 {
		t.Fatalf("highlights = %+v, want declaration and call only", ranges)
	}
	if ranges[0].StartLine != 2 || ranges[1].StartLine != 3 {
		t.Fatalf("highlights = %+v, want lines 2 and 3", ranges)
	}
}

func TestFoldingRangesUseSymbolBodies(t *testing.T) {
	src := []byte("package main\n\nfunc greet() {\n\tprintln(\"hello\")\n}\n")
	ranges := FoldingRanges("main.go", src)
	if len(ranges) != 1 || ranges[0].StartLine != 2 || ranges[0].EndLine != 4 {
		t.Fatalf("folds = %+v, want function body", ranges)
	}
}

func TestSemanticTokensUseHighlightQueries(t *testing.T) {
	src := []byte("package main\n\nfunc greet(name string) string { return \"hi \" + name }\n")
	tokens := SemanticTokens("main.go", src)
	if len(tokens) == 0 {
		t.Fatal("expected semantic tokens")
	}
	want := map[string]bool{"keyword": false, "function": false, "string": false}
	for _, token := range tokens {
		if _, ok := want[token.Type]; ok {
			want[token.Type] = true
		}
	}
	for tokenType, found := range want {
		if !found {
			t.Fatalf("semantic tokens = %+v, missing %s", tokens, tokenType)
		}
	}
}
