package codemode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

var codeTool tool.Tool

func TestMain(m *testing.M) {
	// One compiled sandbox for the package, cached across test runs.
	codeTool = Tools(&Options{CacheDir: filepath.Join(os.TempDir(), "wingman-codemode-test")})[0]
	os.Exit(m.Run())
}

// fakeToolbox serves tools directly and records how they were called.
type fakeToolbox struct {
	tools []tool.Tool

	mu    sync.Mutex
	calls []string
}

func (b *fakeToolbox) Tools() []tool.Tool { return b.tools }

func (b *fakeToolbox) Call(ctx context.Context, name string, args map[string]any) tool.Result {
	b.mu.Lock()
	b.calls = append(b.calls, name)
	b.mu.Unlock()
	for _, t := range b.tools {
		if t.Name == name {
			result, err := t.Execute(ctx, args)
			if err != nil {
				return tool.Error("error: " + err.Error())
			}
			return result
		}
	}
	return tool.Error("error: unknown tool " + name)
}

func textTool(name string, effect tool.Effect, params map[string]any, run func(ctx context.Context, args map[string]any) (string, error)) tool.Tool {
	required := make([]string, 0, len(params))
	for key := range params {
		if !strings.HasSuffix(key, "?") {
			required = append(required, key)
		}
	}
	properties := map[string]any{}
	for key, schema := range params {
		properties[strings.TrimSuffix(key, "?")] = schema
	}
	return tool.Tool{
		Name: name, Description: name + " tool", Effect: tool.StaticEffect(effect),
		Parameters: map[string]any{"type": "object", "properties": properties, "required": required},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			out, err := run(ctx, args)
			return tool.Text(out), err
		},
	}
}

var str = map[string]any{"type": "string"}

func runCode(t *testing.T, box tool.Toolbox, code string) tool.Result {
	t.Helper()
	ctx := t.Context()
	if box != nil {
		ctx = tool.WithToolbox(ctx, box)
	}
	result, err := codeTool.Execute(ctx, map[string]any{"code": code})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

func TestScriptChainsTools(t *testing.T) {
	files := map[string]string{"a.txt": "alpha\nTODO one", "b.txt": "beta", "c.txt": "TODO two\nTODO three"}
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("glob", tool.EffectReadOnly, map[string]any{"pattern": str}, func(context.Context, map[string]any) (string, error) {
			return "a.txt\nb.txt\nc.txt", nil
		}),
		textTool("read", tool.EffectReadOnly, map[string]any{"path": str, "limit?": map[string]any{"type": "integer"}}, func(_ context.Context, args map[string]any) (string, error) {
			content, ok := files[fmt.Sprint(args["path"])]
			if !ok {
				return "", fmt.Errorf("no such file: %v", args["path"])
			}
			return content, nil
		}),
		// MCP names may hold characters Python identifiers cannot.
		textTool("issues_list-open", tool.EffectReadOnly, nil, func(context.Context, map[string]any) (string, error) { return "none", nil }),
		// A tool named like a Python builtin is the tool, not the builtin.
		textTool("exec", tool.EffectMutates, map[string]any{"command": str}, func(_ context.Context, args map[string]any) (string, error) {
			return "ran " + fmt.Sprint(args["command"]), nil
		}),
	}}
	result := runCode(t, box, `
paths = (await glob(pattern="*.txt")).splitlines()
texts = await asyncio.gather(*[read(path=p) for p in paths])
counts = {p: len(re.findall("TODO", t)) for p, t in zip(paths, texts)}
print("scanned", len(paths), "files;", await issues_list_open(), "/", await exec(command="ls"))
{p: n for p, n in counts.items() if n}
`)
	if result.IsError || result.Content.Text() != "scanned 3 files; none / ran ls\n"+`{"a.txt":1,"c.txt":2}` {
		t.Fatalf("result = %q", result.Content.Text())
	}
	if got := len(box.calls); got != 6 {
		t.Fatalf("calls = %v", box.calls)
	}
}

func TestReadOnlyCallsOverlapAndOthersDoNot(t *testing.T) {
	var running, peak atomic.Int32
	slow := func(context.Context, map[string]any) (string, error) {
		now := running.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		running.Add(-1)
		return "ok", nil
	}
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("look", tool.EffectReadOnly, nil, slow),
		textTool("change", tool.EffectMutates, nil, slow),
	}}

	start := time.Now()
	if result := runCode(t, box, "import asyncio\nawait asyncio.gather(look(), look(), look())"); result.IsError {
		t.Fatal(result.Content.Text())
	}
	if took := time.Since(start); peak.Load() != 3 || took > 400*time.Millisecond {
		t.Fatalf("read-only calls did not overlap: peak %d in %v", peak.Load(), took)
	}

	peak.Store(0)
	if result := runCode(t, box, "import asyncio\nawait asyncio.gather(change(), change(), look())"); result.IsError {
		t.Fatal(result.Content.Text())
	}
	if peak.Load() != 1 {
		t.Fatalf("a changing call overlapped another: peak %d", peak.Load())
	}
}

func TestToolFailuresAreExceptions(t *testing.T) {
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("read", tool.EffectReadOnly, map[string]any{"path": str}, func(_ context.Context, args map[string]any) (string, error) {
			return "", fmt.Errorf("no such file: %v", args["path"])
		}),
	}}
	caught := runCode(t, box, `
try:
    await read(path="nope")
    found = True
except RuntimeError as e:
    found = "no such file" in str(e)
found
`)
	if caught.IsError || caught.Content.Text() != "true" {
		t.Fatalf("caught = %q", caught.Content.Text())
	}

	uncaught := runCode(t, box, "print('before')\nawait read(path='nope')")
	text := uncaught.Content.Text()
	if !uncaught.IsError || !strings.HasPrefix(text, "before\n") || !strings.Contains(text, "RuntimeError: error: no such file: nope") || !strings.Contains(text, "line 2") {
		t.Fatalf("uncaught = %q", text)
	}
}

func TestRejectedScriptRunsNothing(t *testing.T) {
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("write", tool.EffectMutates, map[string]any{"path": str, "content": str}, func(context.Context, map[string]any) (string, error) {
			return "written", nil
		}),
		func() tool.Tool {
			ask := textTool("ask", tool.EffectReadOnly, map[string]any{"question": str}, func(context.Context, map[string]any) (string, error) { return "yes", nil })
			ask.Interactive = true
			return ask
		}(),
		{Name: "secret", Hidden: true, Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Text("hidden"), nil }},
	}}
	for name, code := range map[string]string{
		"positional":  `await write("a.txt", "x")`,
		"wrong type":  `await write(path="a.txt", content=3)`,
		"missing":     `await write(path="a.txt")`,
		"late error":  "await write(path='a.txt', content='x')\nawait write(path=1, content='x')",
		"interactive": `await ask(question="ok?")`,
		"hidden":      `await secret()`,
		"not awaited": "text = write(path='a.txt', content='x')\ntext.upper()",
		"bare call":   "write(path='a.txt', content='x')",
	} {
		result := runCode(t, box, code)
		if !result.IsError || !strings.HasPrefix(result.Content.Text(), "The script did not run") {
			t.Errorf("%s: result = %q", name, result.Content.Text())
		}
		// A wrong call comes back with the signature to correct it by.
		shown := strings.Contains(result.Content.Text(), "async def write(*, content: str, path: str) -> str")
		if called := strings.Contains(code, "write("); shown != called {
			t.Errorf("%s: signature shown = %t: %q", name, shown, result.Content.Text())
		}
	}
	if len(box.calls) != 0 {
		t.Fatalf("rejected scripts made calls: %v", box.calls)
	}
}

func TestWarningsDoNotStopAScript(t *testing.T) {
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("read", tool.EffectReadOnly, map[string]any{"path": str}, func(context.Context, map[string]any) (string, error) { return "text", nil }),
	}}
	// The checker calls the condition redundant; that is style, not a mistake.
	result := runCode(t, box, "pages = [1]\nif pages:\n    out = await read(path='a')\nout")
	if result.IsError || result.Content.Text() != "text" {
		t.Fatalf("result = %q", result.Content.Text())
	}
	// Valid Python the checker types wrongly: values of two types in one dict.
	mixed := runCode(t, box, `
stats = {"count": 0, "names": []}
for name in ["a", "b"]:
    stats["count"] += 1
    stats["names"].append(name)
stats
`)
	if mixed.IsError || mixed.Content.Text() != `{"count":2,"names":["a","b"]}` {
		t.Fatalf("mixed dict = %q", mixed.Content.Text())
	}
	for diagnostic, want := range map[string]bool{
		"script.py:4:12: warning[redundant-condition] always truthy":                                   false,
		"script.py:18:5: error[unsupported-operator] Operator `+=` is not supported":                   false,
		"script.py:20:5: error[unresolved-attribute] Attribute `append` is not defined on `int`":       false,
		"script.py:1:1: warning[unused-awaitable] Object of type `CoroutineType` is not awaited":       true,
		"script.py:2:1: error[unresolved-attribute] Object of type `CoroutineType[Any, Any, str]` ...": true,
		"script.py:1:1: error[missing-argument] No argument provided":                                  true,
		"script.py:2:6: error[unresolved-import] Cannot resolve imported module `functions`":           true,
		"script.py:20:56: error[unresolved-reference] Name `next_page` used when not defined":          true,
	} {
		if blocking(diagnostic) != want {
			t.Errorf("blocking(%q) = %t", diagnostic, !want)
		}
	}
}

func TestRunawayLoopStopsEarly(t *testing.T) {
	var calls atomic.Int32
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("page", tool.EffectReadOnly, map[string]any{"n": map[string]any{"type": "integer"}}, func(context.Context, map[string]any) (string, error) {
			calls.Add(1)
			return "0", nil
		}),
	}}
	// The exit condition never holds: the tool answers "0", never "".
	result := runCode(t, box, "n = 1\nwhile True:\n    nxt = await page(n=n)\n    if nxt == '':\n        break\n    n = int(nxt)")
	if !result.IsError || !strings.Contains(result.Content.Text(), "suspension limit") || !strings.Contains(result.Content.Text(), "A loop probably never ends") {
		t.Fatalf("result = %q", result.Content.Text())
	}
	if n := calls.Load(); n < 100 || n > maxHostCalls {
		t.Fatalf("the loop made %d calls", n)
	}
}

func TestOutputSchemaReturnsValues(t *testing.T) {
	issues := tool.Tool{
		Name: "issues", Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"number": map[string]any{"type": "integer"}, "title": str},
				"required":   []any{"number", "title"},
			}}},
			"required": []any{"items"},
		},
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text(`{"items":[{"number":7,"title":"flaky"},{"number":12,"title":"slow"}]}`), nil
		},
	}
	result := runCode(t, &fakeToolbox{tools: []tool.Tool{issues}}, `[i["title"] for i in (await issues())["items"] if i["number"] > 10]`)
	if result.IsError || result.Content.Text() != `["slow"]` {
		t.Fatalf("result = %q", result.Content.Text())
	}

	// The model is told the shape up front: a tool's schema does not say it.
	plain := textTool("read", tool.EffectReadOnly, map[string]any{"path": str}, nil)
	offered := Annotate([]tool.Tool{codeTool, issues, plain})
	for _, want := range []string{
		"These tools return values, not text:",
		"class IssuesResultItemsItem(TypedDict):\n    number: int\n    title: str",
		"async def issues() -> IssuesResult: ...",
	} {
		if !strings.Contains(offered[0].Description, want) {
			t.Errorf("description lacks %q:\n%s", want, offered[0].Description)
		}
	}
	if strings.Contains(offered[0].Description, "def read") || codeTool.Description == offered[0].Description {
		t.Errorf("description lists text tools or changed in place:\n%s", offered[0].Description)
	}
	if same := Annotate([]tool.Tool{codeTool, plain}); same[0].Description != codeTool.Description {
		t.Error("description changed without value tools")
	}
	if same := Annotate([]tool.Tool{issues, plain}); len(same) != 2 {
		t.Error("tools changed without the code tool")
	}
}

func TestUnawaitedCallsFinish(t *testing.T) {
	var done atomic.Bool
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("write", tool.EffectMutates, map[string]any{"path": str}, func(context.Context, map[string]any) (string, error) {
			time.Sleep(100 * time.Millisecond)
			done.Store(true)
			return "written", nil
		}),
	}}
	// A bare call statement is rejected by the checker; a kept one is not.
	result := runCode(t, box, "pending = write(path='a.txt')\n'finished'")
	if !done.Load() {
		t.Fatal("the call was abandoned")
	}
	if result.IsError || !strings.HasPrefix(result.Content.Text(), "finished\nnote: 1 tool call(s) were started without await") {
		t.Fatalf("result = %q", result.Content.Text())
	}
}

func TestFailedScriptStopsItsCalls(t *testing.T) {
	stopped := make(chan struct{})
	box := &fakeToolbox{tools: []tool.Tool{
		textTool("wait", tool.EffectReadOnly, nil, func(ctx context.Context, _ map[string]any) (string, error) {
			select {
			case <-ctx.Done():
				close(stopped)
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
				return "late", nil
			}
		}),
	}}
	start := time.Now()
	result := runCode(t, box, "pending = wait()\nraise ValueError('boom')")
	if !result.IsError || !strings.Contains(result.Content.Text(), "ValueError: boom") {
		t.Fatalf("result = %q", result.Content.Text())
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the running call was not cancelled")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v", took)
	}
}

func TestOutputShapes(t *testing.T) {
	box := &fakeToolbox{}
	for code, want := range map[string]string{
		"x = 1":                              "(no output: end the script with an expression or print what you need)",
		"'plain text'":                       "plain text",
		"print('a')\nprint('b')":             "a\nb",
		"{'t': (1, 2), 's': {3}, 'n': None}": `{"n":null,"s":[3],"t":[1,2]}`,
		"import datetime\ndatetime.date(2026, 1, 2)": `"2026-01-02"`,
		"1 / 3 > 0.3": "true",
	} {
		if result := runCode(t, box, code); result.IsError || result.Content.Text() != want {
			t.Errorf("%q: result = %q", code, result.Content.Text())
		}
	}
}

func TestLimitsAndMisuse(t *testing.T) {
	if _, err := codeTool.Execute(t.Context(), map[string]any{"code": "1"}); err == nil || !strings.Contains(err.Error(), "inside another tool call") {
		t.Fatalf("without a toolbox: %v", err)
	}
	if _, err := codeTool.Execute(tool.WithToolbox(t.Context(), &fakeToolbox{}), map[string]any{"code": "  "}); err == nil {
		t.Fatal("empty code was accepted")
	}
	// No files and no network outside the tools.
	denied := runCode(t, &fakeToolbox{}, "from pathlib import Path\nPath('/etc/passwd').read_text()")
	if !denied.IsError || !strings.Contains(denied.Content.Text(), "PermissionError") {
		t.Fatalf("file read: %q", denied.Content.Text())
	}
	cancelled, cancel := context.WithTimeout(tool.WithToolbox(t.Context(), &fakeToolbox{}), 200*time.Millisecond)
	defer cancel()
	if _, err := codeTool.Execute(cancelled, map[string]any{"code": "while True:\n    pass"}); err == nil {
		t.Fatal("a cancelled script returned a result")
	}
}

// A tool that asks for approval asks from inside a script as it does when
// called directly: the script waits, and a refusal is an exception.
func TestNestedApprovals(t *testing.T) {
	var asked []string
	answer := make(chan bool)
	elicit := &tool.Elicitation{Confirm: func(_ context.Context, message string) (bool, error) {
		asked = append(asked, message)
		return <-answer, nil
	}}
	deploy := textTool("deploy", tool.EffectDangerous, map[string]any{"target": str}, nil)
	deploy.Execute = func(ctx context.Context, args map[string]any) (tool.Result, error) {
		verdict, err := elicit.Approve(ctx, tool.Approval{Message: fmt.Sprintf("Deploy to %v?", args["target"])})
		if err != nil {
			return tool.Result{}, err
		}
		if err := verdict.Err("deploy was not approved"); err != nil {
			return tool.Result{}, err
		}
		return tool.Text(fmt.Sprintf("deployed to %v", args["target"])), nil
	}
	box := &fakeToolbox{tools: []tool.Tool{deploy}}
	script := `
results = []
for target in ["staging", "prod"]:
    try:
        results.append(await deploy(target=target))
    except RuntimeError as e:
        results.append("refused: " + str(e))
results
`
	go func() {
		// The person takes their time; the script's time limit is not running.
		time.Sleep(300 * time.Millisecond)
		answer <- true
		answer <- false
	}()
	result := runCode(t, box, script)
	if result.IsError || result.Content.Text() != `["deployed to staging","refused: error: deploy was not approved"]` {
		t.Fatalf("result = %q", result.Content.Text())
	}
	if len(asked) != 2 || asked[0] != "Deploy to staging?" || asked[1] != "Deploy to prod?" {
		t.Fatalf("asked = %q", asked)
	}
}
