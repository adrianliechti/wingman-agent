package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

// These tests cross the public Agent API, Responses HTTP/SSE encoding, actual
// tool execution, compaction, and the on-disk session journal. The provider is
// deterministic so failures do not depend on a model's phrasing or cache policy.
type contextRequest struct {
	Model        string          `json:"model"`
	Stream       bool            `json:"stream"`
	Instructions json.RawMessage `json:"instructions"`
	Tools        json.RawMessage `json:"tools"`
	Input        json.RawMessage `json:"input"`
	CacheKey     string          `json:"prompt_cache_key"`
	Reasoning    json.RawMessage `json:"reasoning"`
}

type contextProvider struct {
	mu       sync.Mutex
	requests []contextRequest
}

func newContextProvider(t *testing.T, respond func(http.ResponseWriter, contextRequest)) *contextProvider {
	t.Helper()
	p := &contextProvider{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req contextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.requests = append(p.requests, req)
		p.mu.Unlock()
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		respond(w, req)
	}))
	t.Cleanup(server.Close)
	t.Setenv("WINGMAN_URL", server.URL)
	return p
}

func (p *contextProvider) snapshot() []contextRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contextRequest(nil), p.requests...)
}

func requestItems(t *testing.T, req contextRequest) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func assertRequestPrefix(t *testing.T, before, after contextRequest) {
	t.Helper()
	if !bytes.Equal(before.Instructions, after.Instructions) || !bytes.Equal(before.Tools, after.Tools) || before.CacheKey != after.CacheKey {
		t.Fatal("instructions, tools, or cache key changed")
	}
	oldItems, newItems := requestItems(t, before), requestItems(t, after)
	if len(newItems) < len(oldItems) {
		t.Fatal("input prefix became shorter")
	}
	for i := range oldItems {
		if !bytes.Equal(oldItems[i], newItems[i]) {
			t.Fatalf("model-visible input %d changed across requests", i)
		}
	}
}

// Compaction asks the session model for a checkpoint over the full history.
const checkpointRequest = "Context checkpoint: your reply replaces the conversation above."

func isCheckpointRequest(req contextRequest) bool {
	return req.Stream && bytes.Contains(req.Input, []byte(checkpointRequest))
}

func incompleteText(text string) string {
	return fmt.Sprintf("data: {\"type\":\"response.incomplete\",\"sequence_number\":1,\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"incomplete\",\"content\":[{\"type\":\"output_text\",\"text\":%q,\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", text)
}

func writeSummary(w http.ResponseWriter, summary string) {
	encoded, _ := json.Marshal(summary)
	fmt.Fprintf(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}]}`, encoded)
}

func withUsage(body string, input, output int) string {
	body = strings.ReplaceAll(body, `"input_tokens":1`, fmt.Sprintf(`"input_tokens":%d`, input))
	return strings.ReplaceAll(body, `"output_tokens":1`, fmt.Sprintf(`"output_tokens":%d`, output))
}

func TestContextCacheE2ERoundsTurnsUpdatesAndRestart(t *testing.T) {
	var rounds atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !req.Stream {
			t.Error("ample-context session unexpectedly compacted")
			writeSummary(w, "unexpected")
			return
		}
		n := rounds.Add(1)
		if n <= 22 {
			fmt.Fprint(w, completedToolCall(fmt.Sprintf("c%d", n), "read"))
		} else {
			fmt.Fprint(w, completedText("done"))
		}
	})
	var guidance atomic.Value
	guidance.Store("Project: use Go.\nMemory: prefer short answers.")
	var calls atomic.Int64
	cfg := e2eConfig(t, tool.Tool{
		Name: "read", Description: "Read a large file", Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			if calls.Add(1) == 8 {
				guidance.Store("Project: use Go.\nMemory: prefer detailed answers.")
			}
			return tool.Text(strings.Repeat("file contents\n", 650)), nil
		},
	})
	cfg.CacheKey = "cache-e2e"
	cfg.ContextWindow = 400_000
	cfg.ContextInstructions = func() string { return guidance.Load().(string) }
	dir := t.TempDir()
	journal, err := session.OpenJournal(dir, cfg.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent.Agent{Config: cfg, Recorder: journal}
	if err := drain(t, a, "inspect the code"); err != nil {
		t.Fatal(err)
	}
	// A complete replacement explicitly removes the Memory section.
	guidance.Store("Project: use Go.")
	if err := drain(t, a, "now explain"); err != nil {
		t.Fatal(err)
	}
	state := a.StateSnapshot()
	if state.ContextRevision != 0 {
		t.Fatal("old results were rewritten without context pressure")
	}
	if calls.Load() != 22 {
		t.Fatalf("tools executed %d times", calls.Load())
	}
	loaded, err := session.Load(dir, cfg.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	restored := &agent.Agent{Config: cfg}
	if err := restored.Restore(loaded.State); err != nil {
		t.Fatal(err)
	}
	if err := drain(t, restored, "continue after restart"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	firstItems := requestItems(t, requests[0])
	if !bytes.Contains(firstItems[0], []byte("[Session context]")) {
		t.Fatal("initial session context must precede the user input")
	}
	if len(requests) != 25 {
		t.Fatalf("requests = %d, want 25", len(requests))
	}
	for i := 1; i < len(requests); i++ {
		assertRequestPrefix(t, requests[i-1], requests[i])
	}
	last := string(requests[len(requests)-1].Input)
	if strings.Count(last, "[Session context]") != 3 || !strings.Contains(last, "omitted sections no longer apply") {
		t.Fatal("context updates were missing, duplicated on restore, or lacked removal semantics")
	}
	if strings.Contains(last, "earlier tool output trimmed") {
		t.Fatal("historical output was trimmed")
	}
}

func TestContextCompactionE2ERepeatedOverflowAndDiskResume(t *testing.T) {
	var modelRequests, summaryRequests atomic.Int64
	priorCheckpoint := "Implemented parser; tests passed. " + strings.Repeat("Keep the original constraint. ", 80) + " CHECKPOINT_TAIL_MUST_SURVIVE"
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if isCheckpointRequest(req) {
			n := summaryRequests.Add(1)
			if n == 2 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"summarizer context too long"}}`)
				return
			}
			if n > 1 && !strings.Contains(string(req.Input), "CHECKPOINT_TAIL_MUST_SURVIVE") {
				t.Error("prior checkpoint tail lost")
			}
			fmt.Fprint(w, completedText(priorCheckpoint))
			return
		}
		if !req.Stream {
			t.Error("fell back to the utility summary")
			writeSummary(w, priorCheckpoint)
			return
		}
		switch modelRequests.Add(1) {
		case 1:
			fmt.Fprint(w, withUsage(completedToolCall("read-1", "read"), 5400, 200))
		case 2:
			fmt.Fprint(w, withUsage(completedText("first phase complete"), 2500, 1000))
		default:
			fmt.Fprint(w, withUsage(completedText("done"), 500, 1))
		}
	})
	cfg := e2eConfig(t, tool.Tool{Name: "read", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Text(strings.Repeat("observed output\n", 550)), nil
	}})
	cfg.CacheKey = "compact-e2e"
	cfg.ContextWindow, cfg.ReserveTokens = 6000, 500
	cfg.ContextInstructions = func() string { return "Current project guidance" }
	dir := t.TempDir()
	journal, err := session.OpenJournal(dir, cfg.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	goal := "Original goal: retain the API. " + strings.Repeat("constraint ", 240) + " EXACT_USER_TAIL"
	a := &agent.Agent{Config: cfg, Messages: []agent.Message{
		{Role: agent.RoleUser, InputID: "original", Content: []agent.Content{{Text: goal}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: strings.Repeat("earlier progress ", 800)}}},
	}}
	if err := journal.AppendEvents(a.StateSnapshot().Events); err != nil {
		t.Fatal(err)
	}
	a.Recorder = journal
	if err := drain(t, a, "continue implementing"); err != nil {
		t.Fatal(err)
	}
	if a.StateSnapshot().ContextRevision != 1 {
		t.Fatal("new tool output did not trigger compaction before the next round")
	}
	followup := "Now verify it: " + strings.Repeat("specific requirement ", 480)
	if err := drain(t, a, followup); err != nil {
		t.Fatal(err)
	}
	if a.StateSnapshot().ContextRevision != 2 || summaryRequests.Load() != 3 {
		t.Fatalf("checkpoints=%d summary attempts=%d", a.StateSnapshot().ContextRevision, summaryRequests.Load())
	}
	loaded, err := session.Load(dir, cfg.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	restored := &agent.Agent{Config: cfg}
	if err := restored.Restore(loaded.State); err != nil {
		t.Fatal(err)
	}
	if err := drain(t, restored, "continue after resume"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	var streams []contextRequest
	for _, req := range requests {
		if req.Stream && !isCheckpointRequest(req) {
			streams = append(streams, req)
		}
	}
	if len(streams) != 4 {
		t.Fatalf("streaming requests = %d", len(streams))
	}
	for _, index := range []int{1, 2} {
		items := requestItems(t, streams[index])
		for _, item := range items {
			if bytes.Contains(item, []byte("[Session context]")) {
				var message struct {
					Role string `json:"role"`
				}
				if err := json.Unmarshal(item, &message); err != nil {
					t.Fatal(err)
				}
				if message.Role != "user" {
					t.Fatal("session guidance must use a contextual user message; mid-history system messages fail on Anthropic")
				}
			}
		}
		if !bytes.Contains(items[len(items)-1], []byte("[Previous conversation summary]")) {
			t.Fatal("checkpoint must be last after compaction")
		}
		if bytes.Contains(streams[index].Input, []byte(`"function_call"`)) || bytes.Contains(streams[index].Input, []byte(`"function_call_output"`)) {
			t.Fatal("compaction left tool history or orphaned results")
		}
	}
	var gotGoals int
	for _, m := range loaded.State.Context {
		if m.InputID == "original" {
			gotGoals++
			if m.Content[0].Text != goal {
				t.Fatal("original user input was shortened")
			}
		}
	}
	if gotGoals != 1 {
		t.Fatalf("original input retained %d times", gotGoals)
	}
	assertRequestPrefix(t, streams[2], streams[3])
	if !strings.Contains(string(streams[3].Input), "CHECKPOINT_TAIL_MUST_SURVIVE") {
		t.Fatal("disk resume lost the checkpoint")
	}
}

func TestContextImagesAndModelSwitchE2E(t *testing.T) {
	var requests atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !req.Stream {
			t.Error("unexpected compaction from image bytes or stale model usage")
			writeSummary(w, "unexpected")
			return
		}
		n := requests.Add(1)
		if n == 1 {
			fmt.Fprint(w, withUsage(completedText("image inspected"), 100_000, 1))
		} else {
			fmt.Fprint(w, completedText("done"))
		}
	})
	cfg := e2eConfig(t)
	cfg.ContextWindow, cfg.ReserveTokens = 8000, 1000
	model := "model-a"
	cfg.Model = func() string { return model }
	a := &agent.Agent{Config: cfg}
	image := "data:image/png;base64," + strings.Repeat("AAAA", 512*1024)
	stream, err := a.Send(t.Context(), []agent.Content{{Text: "inspect"}, {File: &agent.File{Data: image}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := a.ContextStats().MessagesTokens; got > 2000 {
		t.Fatalf("image counted as encoded bytes: %d", got)
	}
	model = "model-b"
	if err := drain(t, a, "explain with this model"); err != nil {
		t.Fatal(err)
	}
	if got := p.snapshot(); len(got) != 2 || got[1].Model != "model-b" {
		t.Fatal("model switch failed")
	}
}

func TestCompactionFailureE2EPreservesAcceptedInput(t *testing.T) {
	for _, failure := range []string{"empty", "incomplete", "error", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// The session-model checkpoint fails first, then the utility fallback.
			newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				if req.Stream && !isCheckpointRequest(req) {
					t.Error("sent the oversized main request after failed compaction")
					fmt.Fprint(w, completedText("unexpected"))
					return
				}
				switch failure {
				case "empty":
					if req.Stream {
						fmt.Fprint(w, completedText(" \n "))
					} else {
						writeSummary(w, " \n ")
					}
				case "incomplete":
					if req.Stream {
						fmt.Fprint(w, incompleteText("partial"))
					} else {
						fmt.Fprint(w, `{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}`)
					}
				case "error":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"code":"invalid_request_error","message":"summary failed"}}`)
				case "canceled":
					cancel()
				}
			})
			cfg := e2eConfig(t)
			cfg.ContextWindow, cfg.ReserveTokens = 6000, 500
			a := &agent.Agent{Config: cfg, Messages: []agent.Message{
				{Role: agent.RoleUser, Content: []agent.Content{{Text: "original objective"}}},
				{Role: agent.RoleAssistant, Content: []agent.Content{{Text: strings.Repeat("prior evidence ", 2000)}}},
			}}
			stream, err := a.Send(ctx, []agent.Content{{Text: "newly accepted instruction"}})
			if err != nil {
				t.Fatal(err)
			}
			var turnErr error
			for _, err := range stream {
				if err != nil {
					turnErr = err
				}
			}
			if turnErr == nil {
				t.Fatal("compaction failure was hidden")
			}
			state := a.StateSnapshot()
			if state.ContextRevision != 0 || len(state.Context) != 3 || state.Context[2].Content[0].Text != "newly accepted instruction" {
				t.Fatal("failure rewrote history or discarded accepted input")
			}
		})
	}
}

func TestTruncationE2EPersistsOutputBeforeReplay(t *testing.T) {
	var rounds atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if rounds.Add(1) == 1 {
			fmt.Fprint(w, completedToolCall("shell-1", "shell"))
		} else {
			fmt.Fprint(w, completedText("done"))
		}
	})
	output := strings.Repeat("observed line\n", 6000)
	cfg := e2eConfig(t, tool.Tool{Name: "shell", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Text(output), nil }})
	cfg.Hooks.PostToolUse = []hook.PostToolUse{truncation.New(t.TempDir())}
	a := &agent.Agent{Config: cfg}
	if err := drain(t, a, "run the command"); err != nil {
		t.Fatal(err)
	}
	if err := drain(t, a, "explain its output"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	if len(requests) != 3 {
		t.Fatalf("requests=%d", len(requests))
	}
	assertRequestPrefix(t, requests[1], requests[2])
	if got := string(requests[1].Input); !strings.Contains(got, "6000 lines") || !strings.Contains(got, "bytes omitted") || !strings.Contains(got, "Full output saved to:") {
		t.Fatal("recorded output omitted truncation accounting or saved path")
	}
}

func TestContextTrimmingE2EOnlyUnderPressure(t *testing.T) {
	var rounds atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !req.Stream {
			t.Error("trimming should reclaim enough space without a summary")
			writeSummary(w, "unexpected")
			return
		}
		if rounds.Add(1) == 1 {
			fmt.Fprint(w, withUsage(completedText("first pass"), 55_000, 1))
		} else {
			fmt.Fprint(w, withUsage(completedText("done"), 30_000, 1))
		}
	})
	cfg := e2eConfig(t)
	cfg.ContextWindow, cfg.ReserveTokens = 60_000, 5000
	cfg.Hooks.Stop = []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) {
		return hook.Outcome{Block: rounds.Load() == 1}, nil
	}}
	a := &agent.Agent{Config: cfg, Messages: []agent.Message{{Role: agent.RoleUser, Content: []agent.Content{{Text: "original task"}}}}}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old-%d", i)
		a.Messages = append(a.Messages,
			agent.Message{Role: agent.RoleAssistant, Content: []agent.Content{{ToolCall: &agent.ToolCall{ID: id, Name: "read", Args: "{}"}}}},
			agent.Message{Role: agent.RoleAssistant, Content: []agent.Content{{ToolResult: &agent.ToolResult{ID: id, Name: "read", Content: strings.Repeat("x", 8192)}}}},
		)
	}
	if err := drain(t, a, "continue"); err != nil {
		t.Fatal(err)
	}
	if err := drain(t, a, "explain"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	if len(requests) != 3 || a.StateSnapshot().ContextRevision != 1 {
		t.Fatalf("requests=%d checkpoints=%d", len(requests), a.StateSnapshot().ContextRevision)
	}
	if strings.Contains(string(requests[0].Input), "earlier tool output trimmed") || !strings.Contains(string(requests[1].Input), "earlier tool output trimmed") {
		t.Fatal("trim timing did not follow measured pressure")
	}
	assertRequestPrefix(t, requests[1], requests[2])
	for _, m := range a.MessagesSnapshot() {
		for _, c := range m.Content {
			if c.ToolResult != nil && len(c.ToolResult.Content) != 8192 {
				t.Fatal("canonical result was trimmed")
			}
		}
	}
}

func TestContextReactiveOverflowE2EKeepsQueuedSteering(t *testing.T) {
	var rounds atomic.Int64
	var a *agent.Agent
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if !req.Stream {
			writeSummary(w, "Original task remains active; implementation in progress, testing pending.")
			return
		}
		if rounds.Add(1) == 1 {
			if !a.QueueInputWithID([]agent.Content{{Text: "also preserve compatibility"}}, "steering") {
				t.Error("steering rejected")
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"context is full"}}`)
			return
		}
		fmt.Fprint(w, completedText("done"))
	})
	cfg := e2eConfig(t)
	cfg.ContextWindow = -1 // Exercise provider overflow even with auto compaction disabled.
	a = &agent.Agent{Config: cfg, Messages: []agent.Message{
		{Role: agent.RoleUser, Content: []agent.Content{{Text: "original task"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: strings.Repeat("progress ", 1000)}}},
	}}
	if err := drain(t, a, "continue"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	if len(requests) != 4 || a.StateSnapshot().ContextRevision != 1 {
		t.Fatalf("requests=%d checkpoints=%d", len(requests), a.StateSnapshot().ContextRevision)
	}
	if !strings.Contains(string(requests[2].Input), "[Previous conversation summary]") || !strings.Contains(string(requests[3].Input), "also preserve compatibility") {
		t.Fatal("recovery lost checkpoint or accepted steering")
	}
	assertRequestPrefix(t, requests[2], requests[3])
	count := 0
	for _, m := range a.MessagesSnapshot() {
		if m.InputID == "steering" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("steering recorded %d times", count)
	}
}
