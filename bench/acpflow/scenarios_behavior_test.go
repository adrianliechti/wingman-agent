package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A check that accepts its untouched fixture says nothing about the agent.
func TestFileOpsFixtureAndCheck(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := writeFileOpsFixture(root); err != nil {
		t.Fatal(err)
	}
	if err := runGoTests(ctx, root, false, "./..."); err != nil {
		t.Fatalf("fixture must start green: %v", err)
	}
	state := stepState{root: root}
	if err := checkFileOps(ctx, state); err == nil {
		t.Fatal("untouched fixture passed checkFileOps")
	}

	if err := os.Rename(filepath.Join(root, "internal", "util"), filepath.Join(root, "internal", "textutil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "cmd", "shop", "main_helpers.go"), filepath.Join(root, "cmd", "shop", "format.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "internal", "legacy")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"internal/textutil/strings.go", "internal/textutil/strings_test.go", "internal/textutil/slug.go", "internal/textutil/slug_test.go", "catalog/product.go", "cmd/shop/format.go"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		updated := strings.NewReplacer("package util", "package textutil", `internal/util"`, `internal/textutil"`, " util.", " textutil.", "(util.", "(textutil.").Replace(string(data))
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkFileOps(ctx, state); err != nil {
		t.Fatalf("reorganized workspace failed checkFileOps: %v", err)
	}

	// The fixture's trap: replacing every "util." also rewrites timeutil and breaks the build.
	careless := filepath.Join(root, "catalog", "product.go")
	original, err := os.ReadFile(careless)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(careless, []byte(strings.ReplaceAll(string(original), "timeutil.", "timetextutil.")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkFileOps(ctx, state); err == nil {
		t.Fatal("a corrupted timeutil reference passed checkFileOps")
	}
	if err := os.WriteFile(careless, original, 0o644); err != nil {
		t.Fatal(err)
	}

	// A leftover import of the old path must be caught even if it compiles.
	writeTestFile(t, root, "catalog/doc.go", "package catalog\n\n// See example.com/shop/internal/legacy for the old format.\n")
	if err := checkFileOps(ctx, state); err == nil {
		t.Fatal("stale reference passed checkFileOps")
	}
}

func TestModeSwitchFixtureAndChecks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := writeModeSwitchFixture(root); err != nil {
		t.Fatal(err)
	}
	if err := runGoTests(ctx, root, true, "./..."); err == nil {
		t.Fatal("fixture must start with failing tests")
	}
	plan := strings.Repeat("Replace time.Sleep with a timer select in taskqueue/runner.go. ", 4)
	before := treeDigest(root)
	if err := checkPlanOnly(ctx, stepState{root: root, treeBefore: before, finalText: plan}); err != nil {
		t.Fatalf("unchanged tree with a plan failed checkPlanOnly: %v", err)
	}
	if err := checkPlanOnly(ctx, stepState{root: root, treeBefore: before, finalText: "Done."}); err == nil {
		t.Fatal("a turn without a plan passed checkPlanOnly")
	}
	if err := checkModeSwitchBuild(ctx, stepState{root: root, treeBefore: before}); err == nil {
		t.Fatal("unchanged tree passed checkModeSwitchBuild")
	}

	writeTestFile(t, root, "taskqueue/runner.go", `package taskqueue

import (
	"context"
	"time"
)

type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func RunWithRetry(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	delay := cfg.BaseDelay
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if lastErr = fn(ctx); lastErr == nil {
			return nil
		}
		if attempt == cfg.MaxAttempts {
			break
		}
		if cfg.MaxDelay > 0 && delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	return lastErr
}
`)
	if err := checkModeSwitchBuild(ctx, stepState{root: root, treeBefore: before}); err != nil {
		t.Fatalf("fixed implementation failed checkModeSwitchBuild: %v", err)
	}
	if err := checkPlanOnly(ctx, stepState{root: root, treeBefore: before, finalText: plan}); err == nil {
		t.Fatal("a changed tree passed checkPlanOnly")
	}
}

func TestDelegationAndImageFixturesAndChecks(t *testing.T) {
	ctx := context.Background()

	fanout := t.TempDir()
	if err := writeAgentFanoutFixture(fanout); err != nil {
		t.Fatal(err)
	}
	if err := runGoTests(ctx, fanout, false, "./..."); err != nil {
		t.Fatalf("agent_fanout fixture does not build: %v", err)
	}
	if err := checkAgentFanout(ctx, stepState{root: fanout}); err == nil {
		t.Fatal("missing ROUTES.md passed checkAgentFanout")
	}
	var routes strings.Builder
	for service, list := range agentFanoutRoutes {
		routes.WriteString("## " + service + "\n")
		for _, route := range list[:len(list)-1] {
			routes.WriteString("- " + route + "\n")
		}
	}
	writeTestFile(t, fanout, "ROUTES.md", routes.String())
	if err := checkAgentFanout(ctx, stepState{root: fanout}); err == nil {
		t.Fatal("ROUTES.md without the internal routes passed checkAgentFanout")
	}
	for _, list := range agentFanoutRoutes {
		routes.WriteString("- " + list[len(list)-1] + "\n")
	}
	writeTestFile(t, fanout, "ROUTES.md", routes.String())
	if err := checkAgentFanout(ctx, stepState{root: fanout}); err != nil {
		t.Fatalf("complete ROUTES.md failed checkAgentFanout: %v", err)
	}

	fork := t.TempDir()
	if err := writeForkContextFixture(fork); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, fork, "LIMITS.md", "- storage: 64 MiB per object\n- queue: 17 redeliveries\n")
	if err := checkForkContext(ctx, stepState{root: fork}); err == nil {
		t.Fatal("LIMITS.md without the auth limit passed checkForkContext")
	}
	writeTestFile(t, fork, "LIMITS.md", "- storage: 64 MiB per object\n- queue: 17 redeliveries\n- auth: 45 minute tokens\n")
	if err := checkForkContext(ctx, stepState{root: fork}); err != nil {
		t.Fatalf("complete LIMITS.md failed checkForkContext: %v", err)
	}

	flag := t.TempDir()
	if err := writeImageReadFixture(flag); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(flag, "assets", "flag.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoded, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	for i, stripe := range imageReadStripes {
		r, g, b, _ := decoded.At(i*120+60, 120).RGBA()
		if uint8(r>>8) != stripe.color.R || uint8(g>>8) != stripe.color.G || uint8(b>>8) != stripe.color.B {
			t.Fatalf("stripe %d is not %s", i, stripe.name)
		}
	}
	if err := checkImageRead(ctx, stepState{finalText: "Left to right: **Green**, red, blue and yellow."}); err != nil {
		t.Fatalf("correct order failed checkImageRead: %v", err)
	}
	for _, wrong := range []string{"I cannot view images.", "Red, green, blue, yellow.", "green, red, blue"} {
		if err := checkImageRead(ctx, stepState{finalText: wrong}); err == nil {
			t.Fatalf("%q passed checkImageRead", wrong)
		}
	}

	unattended := t.TempDir()
	if err := writeUnattendedFixture(unattended); err != nil {
		t.Fatal(err)
	}
	if err := runGoTests(ctx, unattended, false, "./..."); err != nil {
		t.Fatalf("unattended fixture must start green: %v", err)
	}
	if err := checkUnattended(ctx, stepState{root: unattended}); err == nil {
		t.Fatal("untouched fixture passed checkUnattended")
	}
}

func sse(events ...any) string {
	var b strings.Builder
	for _, event := range events {
		data, _ := json.Marshal(event)
		b.WriteString("data: " + string(data) + "\n\n")
	}
	return b.String()
}

func responsesExchange(t *testing.T, instructions string, input []any, output []any, inputTokens, cachedTokens int) recordedExchange {
	t.Helper()
	request, err := json.Marshal(map[string]any{
		"instructions": instructions,
		"tools":        []any{map[string]any{"type": "function", "name": "edit"}},
		"input":        input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recordedExchange{Status: 200, Path: "/v1/responses", Request: request, Response: sse(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"output": output,
			"usage":  map[string]any{"input_tokens": inputTokens, "output_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": cachedTokens}},
		},
	})}
}

func functionCall(id, name string, args any) map[string]any {
	encoded, _ := json.Marshal(args)
	return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(encoded)}
}

func functionOutput(id string, output any) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": id, "output": output}
}

func TestAnalyzeBehaviorReadsToolChoicesFromResponsesTraffic(t *testing.T) {
	lead := "You are the lead agent. " + strings.Repeat("x", 400)
	user := map[string]any{"type": "message", "role": "user", "content": "reorganize"}
	exchanges := []recordedExchange{
		responsesExchange(t, lead, []any{user}, []any{
			functionCall("c1", "edit", map[string]any{"edits": []any{
				map[string]any{"file_path": "a.go", "move_to": "b.go"},
				map[string]any{"file_path": "old.go", "delete": true},
				map[string]any{"file_path": "c.go", "old_string": "x", "new_string": "y", "delete": false, "move_to": ""},
			}}),
			functionCall("c2", "exec", map[string]any{"command": "git mv x.go y.go && go test ./..."}),
			functionCall("c3", "exec", map[string]any{"command": "go test ./... -run TestRemove"}),
		}, 1000, 0),
		// A subagent speaks with a different prompt and must not count as lead.
		responsesExchange(t, "You are a read-only research specialist.", []any{user}, []any{
			functionCall("s1", "grep", map[string]any{"pattern": "x"}),
		}, 300, 0),
		// A mode that appends to the lead prompt is still the lead.
		responsesExchange(t, lead+"\n\n# Plan mode", []any{
			user,
			functionOutput("c1", "Applied 3 edits across 3 files."),
			functionOutput("c2", "error: plan mode only allows read-only tool calls (exec is blocked in Plan mode)"),
			functionOutput("c3", []any{map[string]any{"type": "input_text", "text": "ok"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA=="}}),
			functionOutput("s1", "a.go:1:x"),
		}, []any{
			functionCall("c4", "agent", map[string]any{"agent_type": "explore", "prompt": "p", "background": true, "fork_context": true}),
		}, 1500, 1200),
	}

	got := analyzeBehavior(exchanges, leadConversation(exchanges))
	if got.ModelRequests != 3 || got.LeadRequests != 2 {
		t.Fatalf("requests = %d lead = %d, want 3 and 2", got.ModelRequests, got.LeadRequests)
	}
	if got.InputTokens != 2800 || got.CachedReadTokens != 1200 {
		t.Fatalf("tokens = %d cached = %d", got.InputTokens, got.CachedReadTokens)
	}
	wantCalls := map[string]int{"edit": 1, "exec": 2, "grep": 1, "agent": 1}
	for name, want := range wantCalls {
		if got.ToolCalls[name] != want {
			t.Errorf("tool_calls[%s] = %d, want %d", name, got.ToolCalls[name], want)
		}
	}
	wantSignals := map[string]int{"edit_move": 1, "edit_delete": 1, "shell_file_op": 1, "plan_blocked": 1, "agent_background": 1, "agent_fork_context": 1}
	for name, want := range wantSignals {
		if got.Signals[name] != want {
			t.Errorf("signals[%s] = %d, want %d", name, got.Signals[name], want)
		}
	}
	if got.ToolErrors["exec"] != 1 || got.ToolErrors["edit"] != 0 {
		t.Errorf("tool_errors = %v, want one failed exec", got.ToolErrors)
	}
	for _, call := range got.Calls {
		if call.ID == "c3" && call.Output != "ok\n[image]" {
			t.Errorf("multi-part output = %q, want text plus an image marker", call.Output)
		}
		if call.ID == "s1" && call.Lead {
			t.Error("subagent call attributed to the lead")
		}
	}
}

func TestAnalyzeBehaviorReadsMessagesTraffic(t *testing.T) {
	request, _ := json.Marshal(map[string]any{
		"system": []any{map[string]any{"type": "text", "text": "You are Claude Code."}},
		"tools":  []any{map[string]any{"name": "Bash"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t0", "is_error": true, "content": []any{map[string]any{"type": "text", "text": "exit 1"}}},
			}},
		},
	})
	response := sse(
		map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 20, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 80, "output_tokens": 1}}},
		map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "t1", "name": "Bash"}},
		map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"command":"rm `}},
		map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `old.go"}`}},
		map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 55}},
	)
	exchanges := []recordedExchange{{Status: 200, Path: "/v1/messages", Request: request, Response: response}}
	got := analyzeBehavior(exchanges, leadConversation(exchanges))
	if got.InputTokens != 1000 || got.CachedReadTokens != 900 || got.OutputTokens != 55 {
		t.Fatalf("usage = in %d cached %d out %d", got.InputTokens, got.CachedReadTokens, got.OutputTokens)
	}
	if got.ToolCalls["Bash"] != 1 || got.Signals["shell_file_op"] != 1 {
		t.Fatalf("calls = %v signals = %v", got.ToolCalls, got.Signals)
	}
}

func TestRequestRecorderForwardsAndStoresInferenceTraffic(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") && r.Header.Get("Accept-Encoding") == "br" {
			t.Error("recorder forwarded the client's Accept-Encoding for a recorded request")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"echo\":" + string(body) + "}\n\n"))
	}))
	defer gateway.Close()
	recorder, err := newRequestRecorder(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	post := func(path string) string {
		request, _ := http.NewRequest(http.MethodPost, recorder.URL()+path, bytes.NewReader([]byte(`{"input":[1]}`)))
		request.Header.Set("Accept-Encoding", "br")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	if got := post("/v1/responses"); !strings.Contains(got, `{"echo":{"input":[1]}}`) {
		t.Fatalf("proxied response = %q", got)
	}
	post("/v1/embeddings")

	exchanges := recorder.snapshot()
	if len(exchanges) != 1 || exchanges[0].Path != "/v1/responses" {
		t.Fatalf("recorded %d exchanges, want only the inference request", len(exchanges))
	}
	if string(exchanges[0].Request) != `{"input":[1]}` || !strings.Contains(exchanges[0].Response, `"echo"`) {
		t.Fatalf("recorded exchange = %+v", exchanges[0])
	}
	path := filepath.Join(t.TempDir(), "run.requests.jsonl")
	if err := recorder.writeJSONL(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); bytes.Count(data, []byte("\n")) != 1 {
		t.Fatalf("JSONL = %q, want one line", data)
	}
}

func TestSummaryLinesUpTwoResultDirectories(t *testing.T) {
	base := t.TempDir()
	write := func(label, name string, result benchResult) {
		t.Helper()
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, base, label+"/"+name, string(data))
	}
	run := func(passed bool, requests int, input, cached int64, signals map[string]int) benchResult {
		result := benchResult{Agent: "wingman", Model: "m", Effort: "medium", Scenario: "file_ops", TestsPassed: passed, TotalMS: 10_000,
			Behavior: &behavior{ModelRequests: requests, LeadRequests: requests, InputTokens: input, CachedReadTokens: cached, Signals: signals, ToolCalls: map[string]int{"agent_wait": 1}}}
		if !passed {
			result.Error = "verification: failed"
		}
		return result
	}
	write("main", "a-r1.json", run(true, 9, 1000, 100, map[string]int{"shell_file_op": 1}))
	write("main", "a-r2.json", run(true, 11, 1000, 300, map[string]int{"shell_file_op": 2}))
	write("branch", "a-r1.json", run(true, 6, 800, 600, map[string]int{"edit_move": 5}))
	write("branch", "a-r2.json", run(false, 8, 800, 600, nil))
	// Sidecars and unrelated JSON in a result directory are not runs.
	writeTestFile(t, base, "branch/a-r1.otel.json", `{"summary":{}}`)
	writeTestFile(t, base, "branch/comparison.json", `{"runs":[]}`)

	var out bytes.Buffer
	if err := writeSummary(&out, []string{filepath.Join(base, "main"), filepath.Join(base, "branch")}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("summary has %d lines, want a header and two rows:\n%s", len(lines), out.String())
	}
	fields := func(line string) []string { return strings.Fields(line) }
	branch, main := fields(lines[1]), fields(lines[2])
	// scenario model agent runs pass seconds requests lead input cached output toolerr signals...
	if branch[2] != "wingman@branch" || branch[4] != "1/2" || branch[6] != "7.0" || branch[9] != "75%" {
		t.Errorf("branch row = %v", branch)
	}
	if main[2] != "wingman@main" || main[4] != "2/2" || main[6] != "10.0" || main[9] != "20%" {
		t.Errorf("main row = %v", main)
	}
	if !strings.Contains(lines[1], "agent_wait=2 edit_move=5") || !strings.Contains(lines[2], "shell_file_op=3") {
		t.Errorf("signals are not summed per row:\n%s", out.String())
	}
}

func TestRepeatScenariosKeepsRepeatedRunsApart(t *testing.T) {
	once := repeatScenarios([]string{"a", " b"}, 1)
	if len(once) != 2 || once[0] != (scenarioRun{name: "a"}) || once[1] != (scenarioRun{name: "b"}) {
		t.Fatalf("single pass = %+v", once)
	}
	twice := repeatScenarios([]string{"a", "b"}, 2)
	want := []scenarioRun{{"a", "-r1"}, {"b", "-r1"}, {"a", "-r2"}, {"b", "-r2"}}
	if len(twice) != len(want) {
		t.Fatalf("two passes = %+v", twice)
	}
	for i := range want {
		if twice[i] != want[i] {
			t.Fatalf("two passes = %+v, want %+v", twice, want)
		}
	}
}
