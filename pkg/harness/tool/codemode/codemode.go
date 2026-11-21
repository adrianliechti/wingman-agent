// Package codemode provides the code tool: the model writes one Python
// script that calls the agent's other tools as functions, so chaining,
// looping over and filtering tool results happens in a sandbox instead of
// one model round trip per call.
package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	monty "github.com/adrianliechti/go-monty"
	"github.com/adrianliechti/go-monty/tools"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const Name = "code"

const (
	// maxExecution bounds interpreter time; time spent in tools does not count.
	maxExecution = time.Minute
	maxMemory    = 256 << 20
	// maxHostCalls bounds the times a script pauses for the host, so a loop
	// that never ends stops after some hundred tool calls, not thousands. A
	// tool call costs one or two.
	maxHostCalls = 1000
	maxSleep     = time.Minute

	maxProgressBytes = 120
	// maxSignatureBytes bounds the return types listed in the description.
	maxSignatureBytes = 6 * 1024

	// prelude runs before every script, so the modules scripts reach for
	// need no import.
	prelude = "import asyncio, datetime, json, math, re"
)

type Options struct {
	// CacheDir keeps the compiled sandbox between processes. Empty uses the
	// user cache directory.
	CacheDir string
}

var description = strings.Join([]string{
	"Run a Python script that calls your other tools as functions and gives you back only what the script computes.",
	"Use it when the script works on the results: it filters or aggregates them, loops over many items, or feeds one tool's result into the next call. Large or numerous intermediate results then never enter the conversation.",
	"Do not use it to batch independent calls whose output you want to read in full: call those tools directly, in parallel. Call a tool directly as well when one call is enough or when you need an image it returns.",
	"- Every other tool is an async function that takes the tool's parameters as keyword arguments: `text = await read(file_path=\"go.mod\")`. It returns the tool's text output as a str, except for the tools listed at the end, which return values. It raises RuntimeError when the tool fails.",
	"- Names follow Python's rules: in tool and parameter names, characters other than letters, digits and `_` become `_` (`-A` is `_A`), and a keyword gets a trailing `_` (`from_`).",
	"- Write plain top-level statements and `await` each call there. Do not wrap the script in `async def main()`, `asyncio.run` or an event loop.",
	"- Run independent calls together: `texts = await asyncio.gather(*[read(file_path=p) for p in paths])`. Read-only calls overlap; calls that change something run one at a time.",
	"- You get back what the script printed and the value of its last expression. Return the answer, not the raw results.",
	"- The script is type-checked against the tool signatures first. A rejected script has run nothing, and the error shows the signatures of the tools it calls.",
	"- The sandbox is a Python subset. asyncio, datetime, json, math and re are already imported; other standard modules of that kind can be. There are no third-party packages, no file or network access except through the tools, no class inheritance and no generators.",
	"- Each call starts fresh: variables do not carry over.",
}, "\n")

// Tools builds the code tool. It reaches the other tools through the
// Toolbox of the call it runs in.
func Tools(opts *Options) []tool.Tool {
	if opts == nil {
		opts = &Options{}
	}
	sandbox := shared(opts.CacheDir)

	return []tool.Tool{{
		Name:        Name,
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectDynamic),
		Composite:   true,
		// The script is bounded by its own limits and each tool by its timeout.
		Timeout: -1,

		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "The Python script. Await tool calls at the top level; end with the expression to return.",
				},
			},
			"required":             []string{"code"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			code, _ := args["code"].(string)
			if strings.TrimSpace(code) == "" {
				return tool.Result{}, errors.New("code is required")
			}
			toolbox := tool.ToolboxFromContext(ctx)
			if toolbox == nil {
				return tool.Result{}, errors.New("code cannot run inside another tool call")
			}
			return run(ctx, sandbox, toolbox, code)
		},
	}}
}

// Annotate adds to the code tool's description the signatures of the tools
// that return values, since a tool's own schema says nothing about its
// result. Apply it to the list offered to the model; without the code tool
// or such tools it returns tools unchanged.
func Annotate(offered []tool.Tool) []tool.Tool {
	index := slices.IndexFunc(offered, func(t tool.Tool) bool { return t.Name == Name && t.Composite })
	if index < 0 {
		return offered
	}
	registry := tools.New()
	var names []string
	for _, t := range offered {
		if t.OutputSchema == nil || !callable(t) {
			continue
		}
		names = append(names, registry.AddSchema(tools.Schema{Name: t.Name, Input: t.Parameters, Output: t.OutputSchema, Async: true}, nil).Name)
	}
	if len(names) == 0 {
		return offered
	}
	// Whole signatures only: the ones over the budget are named instead.
	shown := len(names)
	for shown > 0 && len(registry.Signatures(names[:shown]...)) > maxSignatureBytes {
		shown--
	}
	section := "\nThese tools return values, not text:"
	if shown > 0 {
		section += "\n```python\n" + strings.TrimSpace(registry.Signatures(names[:shown]...)) + "\n```"
	}
	if rest := names[shown:]; len(rest) > 0 {
		section += "\nAlso " + strings.Join(rest, ", ") + ": a rejected script shows their signatures."
	}
	offered = slices.Clone(offered)
	offered[index].Description += section
	return offered
}

// callable reports whether scripts can call t.
func callable(t tool.Tool) bool {
	return t.Name != Name && !t.Hidden && !t.Interactive && t.Execute != nil
}

// sandbox is the compiled interpreter, built on first use.
type sandbox struct {
	cacheDir string

	mu sync.Mutex
	rt *monty.Runtime
}

var (
	sandboxesMu sync.Mutex
	sandboxes   = map[string]*sandbox{}
)

// shared returns the process's sandbox for a cache directory. The compiled
// interpreter is tens of megabytes and serves any number of scripts at once,
// so every session uses the same one, for the life of the process.
func shared(cacheDir string) *sandbox {
	sandboxesMu.Lock()
	defer sandboxesMu.Unlock()
	if sandboxes[cacheDir] == nil {
		sandboxes[cacheDir] = &sandbox{cacheDir: cacheDir}
	}
	return sandboxes[cacheDir]
}

func (s *sandbox) runtime(ctx context.Context) (*monty.Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rt != nil {
		return s.rt, nil
	}
	if report := tool.Progress(ctx); report != nil {
		report("Preparing the Python sandbox")
	}
	dir := s.cacheDir
	if dir == "" {
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, "wingman", "monty")
		}
	}
	var options []monty.RuntimeOption
	if dir != "" {
		options = append(options, monty.WithCacheDir(dir))
	}
	// Compiling takes seconds the first time; a cancelled call must not waste it.
	rt, err := monty.NewRuntime(context.WithoutCancel(ctx), options...)
	if err != nil {
		return nil, fmt.Errorf("start Python sandbox: %w", err)
	}
	s.rt = rt
	return rt, nil
}

func run(ctx context.Context, sandbox *sandbox, toolbox tool.Toolbox, code string) (tool.Result, error) {
	rt, err := sandbox.runtime(ctx)
	if err != nil {
		return tool.Result{}, err
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	calls := &calls{toolbox: toolbox, report: tool.Progress(ctx)}
	registry := tools.New()
	for _, t := range toolbox.Tools() {
		if !callable(t) {
			continue
		}
		output := t.OutputSchema
		if output == nil {
			output = map[string]any{"type": "string"}
		}
		registry.AddSchema(tools.Schema{
			Name: t.Name, Description: t.Description, Input: t.Parameters, Output: output, Async: true,
		}, calls.bind(t))
	}

	session, err := rt.NewSession(ctx, monty.SessionOptions{
		ScriptName:      "script.py",
		TypeCheck:       true,
		TypeCheckStubs:  registry.Stubs(),
		TypeCheckFormat: monty.TypeCheckConcise,
		Limits: &monty.Limits{
			MaxDuration: maxExecution, MaxMemory: maxMemory, MaxSuspensions: maxHostCalls, MaxSleep: maxSleep,
		},
		OSPolicy: monty.OSPolicy{TimeZone: time.Local},
	})
	if err != nil {
		return tool.Result{}, fmt.Errorf("start Python sandbox: %w", err)
	}
	defer session.Close(context.WithoutCancel(ctx))

	if _, err := session.Run(ctx, prelude, monty.RunOptions{}); err != nil {
		return tool.Result{}, fmt.Errorf("start Python sandbox: %w", err)
	}
	var printed bytes.Buffer
	options := registry.Options(monty.RunOptions{Stdout: &printed, Stderr: &printed})
	value, err := session.Run(ctx, code, options)
	if rejected := new(monty.TypingError); errors.As(err, &rejected) && !blocking(rejected.Diagnostics) {
		options.SkipTypeCheck = true
		value, err = session.Run(ctx, code, options)
	}
	// A failed script stops what it started; a finished one lets calls it
	// did not await complete, since their effects were asked for.
	if err != nil {
		cancel()
	}
	unawaited := calls.wait()

	var typing *monty.TypingError
	var exception *monty.Exception
	switch {
	case err == nil:
	case parent.Err() != nil:
		// Interrupted or out of time: the harness words that result.
		return tool.Result{}, parent.Err()
	case errors.As(err, &typing):
		out := "The script did not run: it failed the type check.\n" + strings.TrimSpace(typing.Diagnostics)
		if signatures := registry.Signatures(called(registry, code)...); signatures != "" {
			out += "\n\nSignatures of the tools it calls:\n" + strings.TrimSpace(signatures)
		}
		return tool.Error(out), nil
	case errors.As(err, &exception):
		out := join(printed.String(), strings.TrimSpace(exception.Traceback))
		if strings.Contains(exception.Message, "suspension limit") {
			out = join(out, fmt.Sprintf("The script made too many tool calls (the limit is about %d). A loop probably never ends: check its exit condition against what the tool returns.", maxHostCalls))
		}
		return tool.Error(out), nil
	default:
		return tool.Error(join(printed.String(), "error: "+err.Error())), nil
	}

	out := join(printed.String(), render(value))
	if unawaited > 0 {
		out = join(out, fmt.Sprintf("note: %d tool call(s) were started without await. They ran to completion, but their results were discarded.", unawaited))
	}
	if out == "" {
		out = "(no output: end the script with an expression or print what you need)"
	}
	return tool.Text(out), nil
}

// blockingRules are the checker's findings that stop a script: a call that
// does not fit its signature, a name or module that does not exist, broken
// syntax, and a tool call that is never awaited. Each would fail anyway or
// lose a result. The checker's other complaints are about how ordinary
// Python is typed, where it is often wrong about valid code (a dict with
// values of several types); the interpreter is the judge of those.
var blockingRules = []string{
	"missing-argument", "unknown-argument", "too-many-positional-arguments", "invalid-argument-type",
	"parameter-already-assigned", "no-matching-overload", "call-non-callable",
	"unresolved-reference", "unresolved-import", "invalid-syntax",
	"unused-awaitable",
}

// blocking reports whether diagnostics, one per line in the concise format,
// should stop a script.
func blocking(diagnostics string) bool {
	for _, line := range strings.Split(diagnostics, "\n") {
		// Anything said about a coroutine is a tool call used without await.
		if strings.Contains(line, "CoroutineType") {
			return true
		}
		for _, rule := range blockingRules {
			if strings.Contains(line, "["+rule+"]") {
				return true
			}
		}
	}
	return false
}

// called names the tools a script calls, so a rejected script can be shown
// only their signatures. The empty marker keeps Signatures from listing all.
func called(registry *tools.Registry, code string) []string {
	names := []string{""}
	for _, t := range registry.Tools() {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(t.Name) + `\s*\(`).MatchString(code) {
			names = append(names, t.Name)
		}
	}
	return names
}

// calls runs a script's tool calls, as the model's own parallel calls run:
// read-only ones overlap, the rest one at a time.
type calls struct {
	toolbox tool.Toolbox
	report  func(string)

	order   sync.RWMutex
	running sync.WaitGroup
	active  atomic.Int64
}

func (c *calls) bind(t tool.Tool) tools.SchemaFunc {
	return func(ctx context.Context, args map[string]any) (any, error) {
		c.running.Add(1)
		c.active.Add(1)
		defer c.running.Done()
		defer c.active.Add(-1)

		if t.Effect != nil && t.Effect(args) == tool.EffectReadOnly {
			c.order.RLock()
			defer c.order.RUnlock()
		} else {
			c.order.Lock()
			defer c.order.Unlock()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.report != nil {
			c.report(progress(t.Name, args))
		}

		result := c.toolbox.Call(ctx, t.Name, args)
		output := result.Content.Text()
		if result.IsError {
			return nil, monty.Raise("RuntimeError", "%s", output)
		}
		if t.OutputSchema != nil {
			if value, ok := structured(result.Content); ok {
				return value, nil
			}
		}
		return output, nil
	}
}

// wait blocks until every started call has returned and reports how many
// were still running.
func (c *calls) wait() int {
	active := int(c.active.Load())
	c.running.Wait()
	return active
}

// structured finds the JSON a tool with an output schema returned: the last
// text block that parses.
func structured(content tool.Content) (any, bool) {
	for i := len(content) - 1; i >= 0; i-- {
		if content[i].Text == nil {
			continue
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(*content[i].Text))
		decoder.UseNumber()
		if decoder.Decode(&value) == nil && !decoder.More() {
			return numbers(value), true
		}
	}
	return nil, false
}

// numbers turns json.Number into the int64 or float64 Python expects.
func numbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		f, _ := v.Float64()
		return f
	case []any:
		for i := range v {
			v[i] = numbers(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = numbers(v[k])
		}
	}
	return value
}

func progress(name string, args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil || string(raw) == "{}" {
		return name
	}
	return name + " " + text.TruncateHead(string(raw), maxProgressBytes)
}

func join(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part = strings.TrimRight(part, "\n"); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n")
}

// render shows the script's result: a string as it is, anything else as JSON.
func render(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(plain(value)); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimRight(out.String(), "\n")
}

// plain maps the sandbox's value types onto JSON-encodable ones.
func plain(value any) any {
	switch v := value.(type) {
	case []any:
		return list(v)
	case monty.Tuple:
		return list(v)
	case monty.Set:
		return list(v)
	case monty.FrozenSet:
		return list(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = plain(item)
		}
		return out
	case monty.Dict:
		out := make(map[string]any, len(v))
		for _, item := range v {
			out[fmt.Sprint(item.Key)] = plain(item.Value)
		}
		return out
	case monty.NamedTuple:
		out := make(map[string]any, len(v.Fields))
		for i, field := range v.Fields {
			if i < len(v.Values) {
				out[field] = plain(v.Values[i])
			}
		}
		return out
	case *monty.ClassInstance:
		return plain(v.Attrs)
	case []byte:
		return string(v)
	case monty.Date:
		return fmt.Sprintf("%04d-%02d-%02d", v.Year, int(v.Month), v.Day)
	case time.Duration:
		return v.String()
	case *monty.Exception:
		return v.Type + ": " + v.Message
	case complex128, monty.Time, monty.TimeDelta, monty.FunctionRef, monty.TypeRef, *time.Location:
		return fmt.Sprint(v)
	}
	return value
}

func list(items []any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = plain(item)
	}
	return out
}
