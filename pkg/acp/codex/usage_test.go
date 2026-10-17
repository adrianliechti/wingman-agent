package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func tokenUsageParams(t *testing.T, total, last tokenBreakdown) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tokenUsage": map[string]any{"total": total, "last": last}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func modelItemStarted(kind string) json.RawMessage {
	return json.RawMessage(`{"item":{"id":"item-` + kind + `","type":"` + kind + `"}}`)
}

// Upstream codex-acp 95bc33f: a prompt's usage covers every model request,
// not only the last one, and separates cache writes from fresh input.
func TestPromptUsageCoversEveryModelRequest(t *testing.T) {
	first := tokenBreakdown{TotalTokens: 110, InputTokens: 100, CachedInputTokens: 40, CacheWriteInputTokens: 10, OutputTokens: 10, ReasoningOutputTokens: 5}
	second := tokenBreakdown{TotalTokens: 220, InputTokens: 200, CachedInputTokens: 100, OutputTokens: 20, ReasoningOutputTokens: 10}
	total := first.combine(second, add)

	tokens := newPromptTokenUsage(&tokenBreakdown{})
	d := newEventDispatcher(context.Background(), nil, "s")
	d.tokens = tokens
	d.handleItemStarted(modelItemStarted("reasoning"))
	d.handleTokenUsage(tokenUsageParams(t, first, first))
	// A later backend turn of the same prompt, such as plan implementation,
	// gets its own dispatcher but shares the prompt's usage.
	d = newEventDispatcher(context.Background(), nil, "s")
	d.tokens = tokens
	d.handleTokenUsage(tokenUsageParams(t, total, second))
	d.handleTokenUsage(tokenUsageParams(t, total, second)) // repeated total

	u := d.getUsage()
	if u == nil || u.CachedReadTokens == nil || u.CachedWriteTokens == nil || u.ThoughtTokens == nil {
		t.Fatalf("usage = %#v", u)
	}
	if u.TotalTokens != 330 || u.InputTokens != 150 || *u.CachedReadTokens != 140 || *u.CachedWriteTokens != 10 ||
		u.OutputTokens != 15 || *u.ThoughtTokens != 15 {
		t.Fatalf("usage = %+v (read %d, write %d, thought %d)", u, *u.CachedReadTokens, *u.CachedWriteTokens, *u.ThoughtTokens)
	}
	if sum := u.InputTokens + *u.CachedReadTokens + *u.CachedWriteTokens + u.OutputTokens + *u.ThoughtTokens; sum != u.TotalTokens {
		t.Fatalf("component sum = %d, total = %d", sum, u.TotalTokens)
	}
}

func TestPromptUsageStartsAfterEarlierHistory(t *testing.T) {
	history := tokenBreakdown{TotalTokens: 1000, InputTokens: 900, OutputTokens: 100}
	request := tokenBreakdown{TotalTokens: 150, InputTokens: 120, OutputTokens: 30}

	tokens := newPromptTokenUsage(nil) // resumed thread: total not yet reported
	d := newEventDispatcher(context.Background(), nil, "s")
	d.tokens = tokens
	d.handleItemStarted(modelItemStarted("userMessage"))
	d.handleTokenUsage(tokenUsageParams(t, history, history))
	if u := d.getUsage(); u != nil {
		t.Fatalf("history before model output counted as prompt usage: %+v", u)
	}
	d.handleItemStarted(modelItemStarted("agentMessage"))
	d.handleTokenUsage(tokenUsageParams(t, history.combine(request, add), request))
	if u := d.getUsage(); u == nil || u.TotalTokens != 150 || u.InputTokens != 120 || u.OutputTokens != 30 {
		t.Fatalf("usage = %+v, want only this prompt's request", u)
	}
}

func TestPromptUsageSumsRequestsWhenTotalMovesBack(t *testing.T) {
	tokens := newPromptTokenUsage(&tokenBreakdown{TotalTokens: 5000, InputTokens: 4000, OutputTokens: 1000})
	tokens.observeModelWork()
	first := tokenBreakdown{TotalTokens: 300, InputTokens: 250, OutputTokens: 50}
	second := tokenBreakdown{TotalTokens: 200, InputTokens: 150, OutputTokens: 50}
	tokens.observe(first, first) // total reset below the start, as after compaction
	tokens.observe(first.combine(second, add), second)
	if u := tokens.usage(); u == nil || u.TotalTokens != 500 || u.InputTokens != 400 || u.OutputTokens != 100 {
		t.Fatalf("usage = %+v, want the sum of both requests", u)
	}
}

func TestTokenUsageWithoutTotalKeepsLatestRequest(t *testing.T) {
	d := newEventDispatcher(context.Background(), nil, "s")
	d.tokens = newPromptTokenUsage(&tokenBreakdown{})
	d.handleTokenUsage([]byte(`{"tokenUsage":{"last":{"totalTokens":20,"inputTokens":15,"cachedInputTokens":5,"outputTokens":5}}}`))
	if u := d.getUsage(); u == nil || u.TotalTokens != 20 || u.InputTokens != 10 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestPromptUsageIncludesInitialCompaction(t *testing.T) {
	for _, historyReport := range []string{"none", "before compaction", "during compaction"} {
		t.Run(historyReport, func(t *testing.T) {
			history := tokenBreakdown{TotalTokens: 1000, InputTokens: 900, OutputTokens: 100}
			compact := tokenBreakdown{TotalTokens: 300, InputTokens: 250, OutputTokens: 50}
			request := tokenBreakdown{TotalTokens: 200, InputTokens: 170, OutputTokens: 30}
			d := newEventDispatcher(context.Background(), discardingConn(t), "s")
			d.tokens = newPromptTokenUsage(nil)
			if historyReport == "during compaction" {
				// The RPC reader retained the thread/resume history before the
				// prompt installed its notification consumer.
				d.tokens = newPromptTokenUsage(&history)
			}
			if historyReport == "before compaction" {
				d.handleTokenUsage(tokenUsageParams(t, history, history))
			}
			d.handleItemStarted(modelItemStarted("contextCompaction"))
			if historyReport == "during compaction" {
				// Rate-limit updates at request start repeat historical usage.
				d.handleTokenUsage(tokenUsageParams(t, history, history))
			}
			d.handleTokenUsage(tokenUsageParams(t, history.combine(compact, add), compact))
			d.handleItemStarted(modelItemStarted("agentMessage"))
			d.handleTokenUsage(tokenUsageParams(t, history.combine(compact, add).combine(request, add), request))
			if u := d.getUsage(); u == nil || u.TotalTokens != 500 || u.InputTokens != 420 || u.OutputTokens != 80 {
				t.Fatalf("usage = %+v, want both compaction and ordinary request", u)
			}
		})
	}
}

func TestPromptUsageRefreshesBaselineBeforeModelOutput(t *testing.T) {
	tokens := newPromptTokenUsage(&tokenBreakdown{TotalTokens: 1000, InputTokens: 900, OutputTokens: 100})
	tokens.observe(tokenBreakdown{TotalTokens: 1150, InputTokens: 1020, OutputTokens: 130}, tokenBreakdown{TotalTokens: 150, InputTokens: 120, OutputTokens: 30})
	tokens.observeModelWork()
	tokens.observe(tokenBreakdown{TotalTokens: 1350, InputTokens: 1190, OutputTokens: 160}, tokenBreakdown{TotalTokens: 200, InputTokens: 170, OutputTokens: 30})
	if u := tokens.usage(); u == nil || u.TotalTokens != 200 {
		t.Fatalf("usage = %+v, want only the request after refreshed history", u)
	}
}

func TestThreadUsageRetainsIdleHistoryAndKeepsThreadsSeparate(t *testing.T) {
	client := newCodexClient(newRPCClient(nil, nil))
	history := tokenBreakdown{TotalTokens: 1000, InputTokens: 900, OutputTokens: 100}
	for threadID, total := range map[string]tokenBreakdown{"resumed": history, "other": {TotalTokens: 7000}} {
		params, err := json.Marshal(map[string]any{"threadId": threadID, "tokenUsage": map[string]any{"total": total, "last": total}})
		if err != nil {
			t.Fatal(err)
		}
		// Resumption delivers these updates without an active prompt handler.
		client.dispatchNotification("thread/tokenUsage/updated", params)
	}
	tokens := newPromptTokenUsage(client.threadTokenTotal("resumed"))
	tokens.observeModelWork() // the first model request is compaction
	compact := tokenBreakdown{TotalTokens: 300, InputTokens: 250, OutputTokens: 50}
	tokens.observe(history.combine(compact, add), compact)
	if u := tokens.usage(); u == nil || u.TotalTokens != 300 || u.InputTokens != 250 || u.OutputTokens != 50 {
		t.Fatalf("usage = %+v, want only the resumed thread's compaction request", u)
	}
}

func TestPromptUsageExcludesSyntheticContextCounts(t *testing.T) {
	for _, knownStart := range []bool{false, true} {
		for _, beforeFirstRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("known start=%t, estimate before first request=%t", knownStart, beforeFirstRequest), func(t *testing.T) {
				start := tokenBreakdown{TotalTokens: 5000, InputTokens: 4000, OutputTokens: 1000}
				first := tokenBreakdown{TotalTokens: 200, InputTokens: 150, OutputTokens: 50}
				second := tokenBreakdown{TotalTokens: 300, InputTokens: 250, OutputTokens: 50}
				var baseline *tokenBreakdown
				if knownStart {
					baseline = &start
				}
				tokens := newPromptTokenUsage(baseline)
				tokens.observeModelWork()
				if !beforeFirstRequest {
					tokens.observe(start.combine(first, add), first)
				}
				// TokenUsageInfo.fill_to_context_window changes the cumulative
				// total and zeroes all input/output components on overflow.
				fakeDelta := 10000 - start.TotalTokens
				if !beforeFirstRequest {
					fakeDelta -= first.TotalTokens
				}
				tokens.observe(tokenBreakdown{TotalTokens: 10000}, tokenBreakdown{TotalTokens: fakeDelta})
				// Rate-limit updates can repeat that synthetic report.
				tokens.observe(tokenBreakdown{TotalTokens: 10000}, tokenBreakdown{TotalTokens: fakeDelta})
				if beforeFirstRequest && tokens.usage() != nil {
					t.Fatal("context estimate counted as usage before any model request")
				}
				tokens.observe(tokenBreakdown{TotalTokens: 10000}.combine(second, add), second)
				want := second
				if !beforeFirstRequest {
					want = first.combine(second, add)
				}
				if got := tokens.usage(); !reflect.DeepEqual(got, want.acpUsage()) {
					t.Fatalf("usage = %+v, want real model spend %+v", got, want.acpUsage())
				}
			})
		}
	}
}
