package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// Opt in explicitly; ordinary test runs never make these inference requests.
// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_CONTEXT_MODELS=gpt-5.6-terra,claude-sonnet-5
// go test ./pkg/agent -run TestLiveContext -v -count=1
func TestLiveContext(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_CONTEXT_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_CONTEXT_MODELS to run real inference")
	}
	if os.Getenv("WINGMAN_URL") == "" {
		t.Fatal("set WINGMAN_URL explicitly for live context tests")
	}
	for i, model := range models {
		t.Run(model, func(t *testing.T) {
			testLiveContext(t, strings.TrimSpace(model), strings.TrimSpace(models[(i+1)%len(models)]))
		})
	}
}

type liveContextRequest struct {
	Model        string          `json:"model"`
	Stream       bool            `json:"stream"`
	Instructions json.RawMessage `json:"instructions"`
	Tools        json.RawMessage `json:"tools"`
	Input        json.RawMessage `json:"input"`
}

// Inspect the bytes the real client consumed without delaying its stream.
// Do not log encrypted reasoning payloads.
type liveContextResponseBody struct {
	io.ReadCloser
	content bytes.Buffer
	inspect func([]byte)
}

func (b *liveContextResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.content.Write(p[:n])
	return n, err
}

func (b *liveContextResponseBody) Close() error {
	if b.inspect != nil {
		b.inspect(b.content.Bytes())
		b.inspect = nil
	}
	return b.ReadCloser.Close()
}

func testLiveContext(t *testing.T, model, switchModel string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	currentModel := model
	cfg.Model = func() string { return currentModel }
	cfg.Effort = func() string { return "high" }
	cfg.CacheKey = fmt.Sprintf("context-live-%s-%d", model, time.Now().UnixNano())
	cfg.MaxTurns = 6
	cfg.Instructions = func() string {
		return "Follow the user's test instructions precisely. A requested action is not evidence it was performed. Report facts from tools accurately and answer briefly.\n" + strings.Repeat("Reference policy: preserve compatibility, report uncertainty, and distinguish proposals from verified results.\n", 100)
	}
	guidance := "Current environment: STAGING."
	cfg.ContextInstructions = func() string { return guidance }
	var calls, handoffs atomic.Int64
	cfg.Tools = func() []tool.Tool {
		return []tool.Tool{{
			Name: "lookup_release", Description: "Return the release code and its actual status.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
			Execute: func(context.Context, map[string]any) (tool.Result, error) {
				calls.Add(1)
				return tool.Text("release_code=kiwi-73; deployment=NOT_DEPLOYED; test_status=NOT_RUN.\n" + strings.Repeat("Reference record: compatibility must remain intact and the deployment is still pending.\n", 400)), nil
			},
		}, {
			Name: "record_handoff", Description: "Record a test handoff receipt. Does not deploy software or run tests.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
			Execute: func(context.Context, map[string]any) (tool.Result, error) {
				handoffs.Add(1)
				currentModel = switchModel
				return tool.Text("handoff_receipt=receipt-29; release_code=kiwi-73; deployment=NOT_DEPLOYED; test_status=NOT_RUN"), nil
			},
		}}
	}
	var mu sync.Mutex
	var requests []liveContextRequest
	reasoningStatuses := make(map[string]int)
	client := openai.NewClient(append(cfg.client.Options, option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req liveContextRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		resp, err := next(r)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body = &liveContextResponseBody{ReadCloser: resp.Body, inspect: func(body []byte) {
				type item struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				}
				check := func(event string, items []item) {
					for _, item := range items {
						if item.Type != "reasoning" {
							continue
						}
						if item.Status != "completed" {
							t.Errorf("%s %s reasoning status=%q; want completed", req.Model, event, item.Status)
						}
						mu.Lock()
						reasoningStatuses[req.Model+"/"+event]++
						mu.Unlock()
					}
				}
				if !req.Stream {
					var response struct {
						Output []item `json:"output"`
					}
					if err := json.Unmarshal(body, &response); err != nil {
						t.Error(err)
						return
					}
					check("response", response.Output)
					return
				}
				for line := range bytes.SplitSeq(body, []byte("\n")) {
					data, ok := bytes.CutPrefix(line, []byte("data: "))
					if !ok || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
						continue
					}
					var event struct {
						Type     string `json:"type"`
						Item     item   `json:"item"`
						Response struct {
							Output []item `json:"output"`
						} `json:"response"`
					}
					if err := json.Unmarshal(data, &event); err != nil {
						t.Error(err)
						continue
					}
					switch event.Type {
					case "response.output_item.done":
						check(event.Type, []item{event.Item})
					case "response.completed":
						check(event.Type, event.Response.Output)
					}
				}
			}}
		}
		return resp, err
	}))...)
	cfg.client = &client
	a := &Agent{Config: cfg}
	defer func() {
		if t.Failed() {
			for _, event := range a.StateSnapshot().Events {
				if event.Type == EventContextCheckpoint {
					t.Logf("checkpoint reason: %s", event.ContextReason)
				}
				if event.Type == EventRunTerminal && event.Usage != nil {
					t.Logf("model request usage: %+v", *event.Usage)
				}
			}
		}
	}()
	run := func(a *Agent, prompt string, attachments ...Content) string {
		t.Helper()
		stream, err := a.Send(ctx, append([]Content{{Text: prompt}}, attachments...))
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range stream {
			if err != nil {
				t.Fatal(err)
			}
		}
		return lastAssistantText(a.MessagesSnapshot())
	}
	fixture := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			fixture.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var encodedImage bytes.Buffer
	if err := png.Encode(&encodedImage, fixture); err != nil {
		t.Fatal(err)
	}
	attachment := Content{File: &File{Name: "color.png", Data: "data:image/png;base64," + base64.StdEncoding.EncodeToString(encodedImage.Bytes())}}
	// A substantive planning problem makes adaptive models exercise thinking.
	// A simple lookup may legitimately produce no reasoning at any effort.
	reply := run(a, "First analyze this hypothetical release schedule without executing it: two identical workers, non-preemptive jobs, A takes 7 units; B 4; C 6 after A; D 5 after A; E 8 after B; F 3 after C and D; G 4 after E and F. Compare competing job orders and give the minimum finish time with one schedule. Then call lookup_release once. Report its actual release code, deployment status and test status separately from the hypothetical plan. Also name the dominant color of the attached image. Do not deploy or test anything.", attachment)
	if calls.Load() != 1 || !strings.Contains(strings.ToLower(reply), "kiwi-73") {
		t.Fatalf("tool round: calls=%d reply=%q", calls.Load(), reply)
	}
	if !strings.Contains(strings.ToLower(reply), "red") {
		t.Fatalf("image did not survive the live tool round: %q", reply)
	}
	guidance = "Current environment: PRODUCTION."
	reply = run(a, "Without calling tools, repeat the release code and current environment. Say whether deployment or testing has happened.")
	if !strings.Contains(strings.ToLower(reply), "kiwi-73") || !strings.Contains(strings.ToLower(reply), "production") {
		t.Fatalf("context update not followed: %q", reply)
	}
	if a.StateSnapshot().ContextRevision != 0 {
		t.Fatal("context was rewritten before pressure")
	}
	mu.Lock()
	beforeCompaction := append([]liveContextRequest(nil), requests...)
	mu.Unlock()
	for i := 1; i < len(beforeCompaction); i++ {
		before, after := beforeCompaction[i-1], beforeCompaction[i]
		if !bytes.Equal(before.Instructions, after.Instructions) || !bytes.Equal(before.Tools, after.Tools) {
			t.Fatal("live instructions or tools changed")
		}
		var oldItems, newItems []json.RawMessage
		if err := json.Unmarshal(before.Input, &oldItems); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(after.Input, &newItems); err != nil {
			t.Fatal(err)
		}
		if len(newItems) < len(oldItems) {
			t.Fatal("live prefix shortened")
		}
		for j := range oldItems {
			if !bytes.Equal(oldItems[j], newItems[j]) {
				t.Fatalf("live input prefix changed at item %d", j)
			}
		}
	}
	if switchModel != model {
		assertLiveReasoning(t, a, model)
		reply = run(a, "Call record_handoff exactly once, then report its receipt, release code, current environment and whether deployment or tests were performed.")
		if handoffs.Load() != 1 || !strings.Contains(reply, "receipt-29") || !strings.Contains(strings.ToLower(reply), "production") {
			t.Fatalf("live handoff during a tool round failed: calls=%d reply=%q", handoffs.Load(), reply)
		}
		// Adaptive models may emit no thinking on this simple continuation.
		// Each direction separately requires real source-model signatures
		// above, before the tool handoff removes them.
		mu.Lock()
		switched := append([]liveContextRequest(nil), requests[len(beforeCompaction):]...)
		mu.Unlock()
		found := false
		for _, req := range switched {
			if req.Stream && req.Model == switchModel {
				if bytes.Contains(req.Input, []byte(`"encrypted_content"`)) || !bytes.Contains(req.Input, []byte(`"function_call_output"`)) {
					t.Fatal("first live request to switched model must remove old signatures and retain tool results")
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatal("live tool continuation did not switch models")
		}
		// Resume raw signed history from disk before switching back. This
		// exercises stored producer/prefix bindings, not only fresh memory.
		path := filepath.Join(t.TempDir(), "state.json")
		snapshot := a.StateSnapshot()
		if err := snapshot.Save(path); err != nil {
			t.Fatal(err)
		}
		var saved State
		if err := saved.Load(path); err != nil {
			t.Fatal(err)
		}
		a = &Agent{Config: cfg}
		if err := a.Restore(saved); err != nil {
			t.Fatal(err)
		}
		currentModel = model
		reply = run(a, "Without tools, repeat the handoff receipt, release code and current environment. Deployment and tests have not been performed.")
		if !strings.Contains(reply, "receipt-29") || !strings.Contains(reply, "kiwi-73") || !strings.Contains(strings.ToLower(reply), "production") {
			t.Fatalf("live switch back after disk resume lost state: %q", reply)
		}
		mu.Lock()
		back := requests[len(requests)-1]
		mu.Unlock()
		if bytes.Contains(back.Input, []byte(`"encrypted_content"`)) {
			t.Fatal("switch back resurrected old thinking signatures")
		}
		t.Logf("raw-history tool handoff and disk resume: %s -> %s -> %s", model, switchModel, model)
	}
	// Lower only the configured window to exercise real auto compaction without
	// buying an entire provider context window's worth of synthetic history.
	measured := a.ContextStats().CurrentTokens
	if measured <= 0 {
		t.Fatal("backend returned no usable context usage")
	}
	cfg.ContextWindow = int(measured) + 500
	cfg.ReserveTokens = 1000
	revisionBefore := a.StateSnapshot().ContextRevision
	t.Logf("forcing auto compaction: measured=%d window=%d", measured, cfg.ContextWindow)
	reply = run(a, "Continue the original task without tools. Reply with four labeled lines: Release code; Current environment; Deployment (PERFORMED or NOT_PERFORMED); Tests (PERFORMED or NOT_PERFORMED). Use the recorded facts, not assumptions.")
	if a.StateSnapshot().ContextRevision != revisionBefore+1 {
		t.Fatalf("live auto compaction: context revision=%d, want %d", a.StateSnapshot().ContextRevision, revisionBefore+1)
	}
	statuses := make(map[string]string)
	for _, line := range strings.Split(reply, "\n") {
		separator := strings.IndexAny(line, ":;")
		if separator < 0 {
			continue
		}
		label := strings.ToLower(strings.Trim(line[:separator], " *`\t"))
		for _, key := range []string{"deployment", "tests"} {
			if strings.HasPrefix(label, key) {
				statuses[key] = strings.ToUpper(strings.Trim(line[separator+1:], " *`\t"))
			}
		}
	}
	if !strings.Contains(strings.ToLower(reply), "kiwi-73") || !strings.Contains(strings.ToLower(reply), "production") || statuses["deployment"] != "NOT_PERFORMED" || statuses["tests"] != "NOT_PERFORMED" {
		t.Fatalf("facts lost or task status overstated through live compaction: %q", reply)
	}
	t.Logf("after compaction: %s", reply)
	checkpoint := ""
	for _, m := range a.requestMessages() {
		if isCompactionSummary(m) {
			checkpoint = contentText(m.Content)
		}
	}
	if !strings.Contains(strings.ToLower(checkpoint), "kiwi-73") {
		t.Fatalf("real summary lost the release code: %q", checkpoint)
	}
	// Reconstruct the journal representation and continue with the actual model.
	encoded, err := json.Marshal(a.StateSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(encoded, &state); err != nil {
		t.Fatal(err)
	}
	restored := &Agent{Config: cfg}
	if err := restored.Restore(state); err != nil {
		t.Fatal(err)
	}
	cfg.ContextWindow = 0
	reply = run(restored, "After resuming, repeat the release code and current environment without using tools.")
	if !strings.Contains(strings.ToLower(reply), "kiwi-73") || !strings.Contains(strings.ToLower(reply), "production") {
		t.Fatalf("live resume lost state: %q", reply)
	}
	recap, err := restored.Recap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(recap), "kiwi-73") {
		t.Fatalf("live recap lost the release code: %q", recap)
	}
	if calls.Load() != 1 {
		t.Fatalf("completed tool work was repeated %d times", calls.Load())
	}
	if switchModel != model {
		currentModel = switchModel
		reply = run(restored, "Without tools, repeat the release code and current environment from this resumed session.")
		if !strings.Contains(strings.ToLower(reply), "kiwi-73") || !strings.Contains(strings.ToLower(reply), "production") {
			t.Fatalf("live switch from %s to %s lost state: %q", model, switchModel, reply)
		}
		t.Logf("model switch %s -> %s passed", model, switchModel)
	}
	usage := restored.UsageSnapshot()
	mu.Lock()
	for _, event := range []string{"response.output_item.done", "response.completed"} {
		if reasoningStatuses[model+"/"+event] == 0 {
			t.Errorf("no live %s reasoning observed for %s", event, model)
		}
	}
	t.Logf("completed reasoning status verified: %v", reasoningStatuses)
	mu.Unlock()
	t.Logf("model=%s input=%d output=%d cache_read=%d cache_write=%d checkpoints=%d", model, usage.InputTokens, usage.OutputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, restored.StateSnapshot().ContextRevision)
	t.Logf("post-compaction reply: %s\nrecap: %s", reply, recap)
}

func assertLiveReasoning(t *testing.T, a *Agent, model string) {
	t.Helper()
	for _, message := range a.requestMessages() {
		for _, content := range message.Content {
			if r := content.Reasoning; r != nil && r.Content != "" && r.Model == model {
				return
			}
		}
	}
	t.Fatalf("live backend returned no replayable reasoning for %s; signature compatibility was not exercised", model)
}
