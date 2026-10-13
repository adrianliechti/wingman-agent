package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

type commandStartClient struct {
	*liveClient
	toolStarted chan struct{}
}

func (c *commandStartClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	if u := n.Update.ToolCall; u != nil && u.Kind == acp.ToolKindExecute && u.Status == acp.ToolCallStatusInProgress {
		select {
		case c.toolStarted <- struct{}{}:
		default:
		}
	}
	return c.liveClient.SessionUpdate(ctx, n)
}

// TestLiveCodexPromptUsageCoversEveryRequest checks that a prompt needing a
// tool call reports the usage of all its model requests, not only the last.
func TestLiveCodexPromptUsageCoversEveryRequest(t *testing.T) {
	client := &liveClient{}
	cc, ctx, cwd, agent := startLiveCodexAgent(t, client, 180*time.Second)

	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	requests := recordLiveUsage(agent, ns.SessionId)
	r, err := cc.Prompt(ctx, acp.PromptRequest{
		SessionId: ns.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Run the shell command `echo usage-check` and then reply with exactly: DONE")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	text, titles := client.snapshot()
	if !strings.Contains(strings.ToUpper(text), "DONE") {
		t.Fatalf("reply = %q, titles = %q", text, titles)
	}
	if r.Usage == nil {
		t.Fatal("prompt response has no usage")
	}

	t.Logf("tools %q; prompt usage %+v", titles, *r.Usage)
	if count := requests.check(t, r.Usage); count < 2 {
		t.Fatalf("the prompt made %d model request(s), want at least two", count)
	}
}

// Independently sum this turn's distinct model-request counts. Rate-limit
// updates repeat the thread total and must not count as another request.
type liveUsageRecorder struct {
	mu          sync.Mutex
	turnID      string
	requests    map[string][]tokenBreakdown
	previous    *tokenBreakdown
	compactions int
}

func recordLiveUsage(agent *Agent, sid acp.SessionId) *liveUsageRecorder {
	r := &liveUsageRecorder{requests: map[string][]tokenBreakdown{}, previous: agent.codex.threadTokenTotal(string(sid))}
	agent.codex.setGlobalNotificationHandler(func(threadID, method string, params json.RawMessage) {
		agent.handleGlobalNotification(threadID, method, params)
		if threadID != string(sid) {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		switch method {
		case "item/started":
			var p struct {
				Item json.RawMessage `json:"item"`
			}
			if json.Unmarshal(params, &p) == nil {
				if _, kind, ok := peekItem(p.Item); ok && kind == "contextCompaction" {
					r.compactions++
				}
			}
		case "turn/started":
			var p struct {
				Turn turn `json:"turn"`
			}
			if json.Unmarshal(params, &p) == nil {
				r.turnID = p.Turn.ID
			}
		case "thread/tokenUsage/updated":
			var p struct {
				TurnID string `json:"turnId"`
				tokenUsageUpdate
			}
			if json.Unmarshal(params, &p) == nil && p.TokenUsage.Total != nil && (r.previous == nil || *r.previous != *p.TokenUsage.Total) {
				r.previous = p.TokenUsage.Total
				if last := p.TokenUsage.Last; last.InputTokens > 0 || last.OutputTokens > 0 {
					r.requests[p.TurnID] = append(r.requests[p.TurnID], last)
				}
			}
		}
	})
	return r
}

func (r *liveUsageRecorder) check(t *testing.T, got *acp.Usage) int {
	t.Helper()
	r.mu.Lock()
	requests := append([]tokenBreakdown(nil), r.requests[r.turnID]...)
	r.mu.Unlock()
	if len(requests) == 0 {
		t.Fatal("no model requests with usage recorded for the latest turn")
	}
	var total tokenBreakdown
	for _, request := range requests {
		total = total.combine(request, add)
	}
	want := total.acpUsage()
	t.Logf("%d distinct model requests: %+v; total %+v", len(requests), requests, total)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompt usage = %+v, want distinct model requests %+v", got, want)
	}
	return len(requests)
}

func TestLiveCodexUsageAfterCancellation(t *testing.T) {
	client := &commandStartClient{liveClient: &liveClient{}, toolStarted: make(chan struct{}, 1)}
	cc, ctx, cwd, agent := startLiveCodexAgent(t, client, 180*time.Second)
	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	requests := recordLiveUsage(agent, ns.SessionId)
	type outcome struct {
		response acp.PromptResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := cc.Prompt(ctx, acp.PromptRequest{
			SessionId: ns.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Run exactly the shell command `echo cancel-usage-check; sleep 90` in the foreground, wait for it to finish, then reply DONE.")},
		})
		done <- outcome{r, err}
	}()
	select {
	case <-client.toolStarted:
	case r := <-done:
		t.Fatalf("prompt ended before a command started: %+v, %v", r.response, r.err)
	case <-ctx.Done():
		t.Fatalf("waiting for a command: %v", ctx.Err())
	}
	if err := cc.Cancel(ctx, acp.CancelNotification{SessionId: ns.SessionId}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.response.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancelled prompt = %+v, %v", r.response, r.err)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for cancellation: %v", ctx.Err())
	}
	r, err := cc.Prompt(ctx, acp.PromptRequest{
		SessionId: ns.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Run the shell command `echo post-cancel-usage-check` and then reply with exactly: DONE")},
	})
	if err != nil || r.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("follow-up prompt = %+v, %v", r, err)
	}
	requests.check(t, r.Usage)
}

func TestLiveCodexUsageWithInitialCompaction(t *testing.T) {
	client := &liveClient{}
	cc, ctx, cwd, agent := startLiveCodexAgent(t, client, 180*time.Second)
	ns, err := cc.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("Reply with exactly: FIRST")}}); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	// This override belongs only to the resumed test thread. A small limit
	// forces compaction before the follow-up's ordinary model output.
	if _, err := cc.CloseSession(ctx, acp.CloseSessionRequest{SessionId: ns.SessionId}); err != nil {
		t.Fatalf("close before resume: %v", err)
	}
	resumed, err := agent.codex.threadResume(ctx, threadResumeParams{
		ThreadID: string(ns.SessionId), Cwd: cwd,
		Config: map[string]any{"model_auto_compact_token_limit": 1},
	})
	if err != nil {
		t.Fatalf("resume with compaction limit: %v", err)
	}
	agent.registerSession(ns.SessionId, resumed.Model, derefEffort(resumed.ReasoningEffort), defaultCollaborationMode, nil)
	requests := recordLiveUsage(agent, ns.SessionId)
	r, err := cc.Prompt(ctx, acp.PromptRequest{SessionId: ns.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("Reply with exactly: SECOND")}})
	if err != nil || r.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("compacting prompt = %+v, %v", r, err)
	}
	requests.mu.Lock()
	compactions := requests.compactions
	requests.mu.Unlock()
	if compactions == 0 {
		t.Fatal("the test thread did not compact")
	}
	t.Logf("compactions: %d", compactions)
	requests.check(t, r.Usage)
}
