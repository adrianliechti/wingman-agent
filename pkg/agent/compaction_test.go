package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
)

func TestSendCompactionHookLifecycle(t *testing.T) {
	for _, reactive := range []bool{false, true} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("reactive=%t/stop=%t", reactive, stop), func(t *testing.T) {
				requests, summaries := 0, 0
				client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{
					Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						var request struct {
							Stream bool            `json:"stream"`
							Input  json.RawMessage `json:"input"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Fatal(err)
						}
						status, contentType := http.StatusOK, "application/json"
						var body string
						if !request.Stream {
							summaries++
							body = `{"output":[` + strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", "summary of earlier work") + `]}`
						} else {
							requests++
							contentType = "text/event-stream"
							body = phaseTestResponse(false, finalAnswerOutput)
							if requests == 1 {
								if reactive {
									status, contentType = http.StatusBadRequest, "application/json"
									body = `{"error":{"code":"context_length_exceeded","message":"context limit reached"}}`
								} else {
									body = strings.ReplaceAll(body, `"input_tokens":1`, `"input_tokens":10`)
								}
							} else if !strings.Contains(string(request.Input), "summary of earlier work") || !strings.Contains(string(request.Input), "compacted session guidance") {
								t.Errorf("next request omitted the summary or compact-session hook context: %s", request.Input)
							}
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					}),
				}))
				var order []string
				a := &Agent{Config: &Config{
					client: &client, MaxTurns: 3, ContextWindow: 10, ReserveTokens: 1,
					Hooks: hook.Hooks{
						SessionStart: []hook.SessionStart{func(_ context.Context, source string) (hook.Outcome, error) {
							order = append(order, source)
							if source == "compact" {
								return hook.Outcome{AdditionalContext: []string{"compacted session guidance"}}, nil
							}
							return hook.Outcome{}, nil
						}},
						PreCompact: []hook.PreCompact{func(context.Context, string) (hook.Outcome, error) {
							order = append(order, "pre")
							return hook.Outcome{}, nil
						}},
						PostCompact: []hook.PostCompact{func(context.Context, string) (hook.Outcome, error) {
							order = append(order, "post")
							return hook.Outcome{Stop: stop}, nil
						}},
						Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) {
							return hook.Outcome{Block: requests == 1}, nil
						}},
					},
				}, Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "earlier work"}}}}}
				stream, err := a.Send(t.Context(), []Content{{Text: "continue the work"}})
				if err != nil {
					t.Fatal(err)
				}
				for _, err := range stream {
					if err != nil {
						t.Fatal(err)
					}
				}
				wantOrder := []string{"startup", "pre", "post", "compact"}
				wantRequests, wantStatus := 2, RuntimeCompleted
				if stop {
					wantOrder = wantOrder[:3]
					wantRequests, wantStatus = 1, RuntimeInterrupted
				}
				if !slices.Equal(order, wantOrder) || summaries != 1 || requests != wantRequests {
					t.Fatalf("hook order=%v summaries=%d requests=%d", order, summaries, requests)
				}
				if terminal := a.Events[len(a.Events)-1].Terminal; terminal.Status != wantStatus {
					t.Fatalf("terminal = %+v, want %s", terminal, wantStatus)
				}
			})
		}
	}
}

func TestShouldCompactProactivelyScalesReserveOnLargeWindows(t *testing.T) {
	a := &Agent{Config: &Config{}}

	// 1M window: a fixed 32k reserve would defer compaction to 968k (~97%);
	// the 10% floor triggers at 900k instead.
	if a.compactionOvershoot("claude-opus-4-8", 899_000) > 0 {
		t.Error("should not compact below the 10% headroom on a 1M window")
	}
	if a.compactionOvershoot("claude-opus-4-8", 901_000) <= 0 {
		t.Error("should compact within 10% of a 1M window")
	}

	// 200k window: the fixed 32k default already exceeds 10% (20k), so it wins.
	if a.compactionOvershoot("claude-opus-4-5", 167_000) > 0 {
		t.Error("should not compact below window-reserve on a 200k window")
	}
	if a.compactionOvershoot("claude-opus-4-5", 169_000) <= 0 {
		t.Error("should compact past window-reserve on a 200k window")
	}
}

func TestShouldCompactProactivelyHonorsExplicitReserve(t *testing.T) {
	a := &Agent{Config: &Config{ReserveTokens: 10_000}}

	// With an explicit 10k reserve the threshold is 990k, not the 900k the
	// window fraction would impose — 905k must not trigger compaction.
	if a.compactionOvershoot("claude-opus-4-8", 905_000) > 0 {
		t.Error("explicit reserve must be honored, not inflated by the window fraction")
	}
	if a.compactionOvershoot("claude-opus-4-8", 995_000) <= 0 {
		t.Error("explicit reserve still triggers once its threshold is crossed")
	}
}

func TestShouldCompactProactivelyDisabled(t *testing.T) {
	if (&Agent{Config: &Config{ContextWindow: -1}}).compactionOvershoot("claude-opus-4-8", 999_999_999) > 0 {
		t.Error("a negative context window disables proactive compaction")
	}
	if (&Agent{Config: &Config{}}).compactionOvershoot("claude-opus-4-8", 0) > 0 {
		t.Error("zero measured tokens must not trigger compaction")
	}
}
