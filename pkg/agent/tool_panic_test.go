package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func panicToolAgent(t *testing.T, parallelism int, calls int) *Agent {
	t.Helper()

	var body strings.Builder
	for i := range calls {
		body.WriteString("data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":" +
			string(rune('0'+i)) + ",\"item\":{\"type\":\"function_call\",\"id\":\"fc_" + string(rune('a'+i)) +
			"\",\"call_id\":\"call_" + string(rune('a'+i)) + "\",\"name\":\"boom\",\"arguments\":\"{}\",\"status\":\"completed\"}}\n\n")
	}
	body.WriteString("data: {\"type\":\"response.completed\",\"sequence_number\":9,\"response\":{\"usage\":{\"input_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")

	client := streamingTestClient(func(*http.Request) string { return body.String() })

	return &Agent{Config: &Config{
		client:           &client,
		MaxTurns:         1,
		MaxParallelTools: parallelism,
		Model:            func() string { return "test-model" },
		Tools: func() []tool.Tool {
			return []tool.Tool{{
				Name:        "boom",
				Description: "panics",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				Effect:      tool.StaticEffect(tool.EffectReadOnly),
				Execute: func(context.Context, map[string]any) (tool.Result, error) {
					panic("tool exploded")
				},
			}}
		},
	}}
}

func TestToolPanicBecomesToolErrorNotProcessCrash(t *testing.T) {
	for _, tc := range []struct {
		name        string
		parallelism int
		calls       int
	}{
		{"parallel", 2, 2},
		{"sequential", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := panicToolAgent(t, tc.parallelism, tc.calls)

			stream, err := a.Send(context.Background(), []Content{{Text: "go"}})
			if err != nil {
				t.Fatal(err)
			}

			var reported int
			for message, err := range stream {
				if err != nil {
					continue
				}
				for _, c := range message.Content {
					if c.ToolResult != nil && strings.Contains(c.ToolResult.Content, "panicked") {
						reported++
					}
				}
			}
			if reported != tc.calls {
				t.Fatalf("panicking tool reported %d contained failures, want %d", reported, tc.calls)
			}
		})
	}
}

func TestDuplicateToolCallIDReportsErrorWhenLeaderPanics(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	tl := tool.Tool{
		Name: "boom",
		Effect: func(map[string]any) tool.Effect {
			close(started)
			<-release
			panic("effect exploded")
		},
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text("ok"), nil
		},
	}
	a := &Agent{Config: &Config{ToolTimeout: -1}}
	tc := ToolCall{ID: "dup-panic", Name: "boom", Args: `{}`}

	results := make(chan tool.Result, 2)
	go func() { results <- a.runSingleToolCall(context.Background(), tc, []tool.Tool{tl}) }()
	<-started
	go func() { results <- a.runSingleToolCall(context.Background(), tc, []tool.Tool{tl}) }()
	close(release)

	first, second := <-results, <-results
	if !first.IsError || !strings.Contains(first.Content, "panicked") {
		t.Fatalf("leader result = %+v, want panic error", first)
	}
	if !second.IsError || !strings.Contains(second.Content, "panicked") {
		t.Fatalf("duplicate waiter result = %+v, want panic error", second)
	}
}
