package agent

import (
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestContextEstimateImagesAndUsage(t *testing.T) {
	image := Message{Role: RoleUser, Content: []Content{{File: &File{Data: "data:image/png;base64," + strings.Repeat("A", 4*1024*1024)}}}}
	if got := messageTokens(image); got != 4+estimatedImageTokens {
		t.Fatalf("image estimate = %d", got)
	}
	if messageBytes(image) < 4*1024*1024 {
		t.Fatal("byte accounting must still measure the actual payload")
	}
	a := &Agent{Config: &Config{}}
	req := &request{model: "model-a", instructions: "base", messages: []Message{image}}
	resp := &response{messages: []Message{{Role: RoleAssistant, Content: []Content{{Text: "answer"}}}}, usage: Usage{
		InputTokens: 2200, OutputTokens: 123, CacheReadInputTokens: 2100,
	}}
	a.anchorContextUsage(req, resp)
	req.messages = append(req.messages, resp.messages...)
	if got := a.requestTokens(req); got != 2323 {
		t.Fatalf("measured input + output = %d, want 2323 (including cached tokens)", got)
	}
	result := Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "c", Content: strings.Repeat("x", 8000)}}}}
	req.messages = append(req.messages, result)
	if got, want := a.requestTokens(req), int64(2323+messageTokens(result)); got != want {
		t.Fatalf("new tool result accounting = %d, want %d", got, want)
	}
	// The next response replaces the anchor; output and results are not added twice.
	a.anchorContextUsage(req, &response{usage: Usage{InputTokens: 5000}})
	if got := a.requestTokens(req); got != 5000 {
		t.Fatalf("recalibrated usage = %d, want 5000", got)
	}
}

func TestContextUsageInvalidation(t *testing.T) {
	for _, change := range []string{"model", "instructions", "tools", "schema", "checkpoint", "restore"} {
		t.Run(change, func(t *testing.T) {
			a := &Agent{Config: &Config{}}
			req := &request{model: "a", instructions: "base", messages: []Message{{Role: RoleUser, Content: []Content{{Text: "hello"}}}}}
			a.anchorContextUsage(req, &response{usage: Usage{InputTokens: 100_000}})
			switch change {
			case "model":
				req.model = "b"
			case "instructions":
				req.instructions = "new base"
			case "tools":
				req.tools = []tool.Tool{{Name: "read"}}
			case "schema":
				req.outputSchema = map[string]any{"type": "object"}
			case "checkpoint":
				if err := a.replaceContext("test rewrite", req.messages); err != nil {
					t.Fatal(err)
				}
			case "restore":
				if err := a.Restore(State{Messages: req.messages, Usage: Usage{LastInputTokens: 100_000}}); err != nil {
					t.Fatal(err)
				}
			}
			_, fixed := requestPrefix(req)
			if got, want := a.requestTokens(req), int64(fixed+messagesTokens(req.messages)); got != want {
				t.Fatalf("stale anchor after %s: %d, want estimate %d", change, got, want)
			}
		})
	}
}

func TestContextStatsShowsCurrentUsageAfterCompaction(t *testing.T) {
	a := &Agent{Config: &Config{Model: func() string { return "a" }}}
	input := Message{Role: RoleUser, Content: []Content{{Text: "task"}}}
	if err := a.appendMessages(input); err != nil {
		t.Fatal(err)
	}
	req := &request{model: "a", messages: a.requestMessages()}
	a.anchorContextUsage(req, &response{usage: Usage{InputTokens: 100_000}})
	if got := a.ContextStats().CurrentTokens; got != 100_000 {
		t.Fatalf("current usage=%d", got)
	}
	if err := a.replaceContext("compact model context", []Message{input, hiddenContextMessage(summaryPrefix + "\nbrief")}); err != nil {
		t.Fatal(err)
	}
	if got := a.ContextStats().CurrentTokens; got >= 1000 {
		t.Fatalf("stale context display after compaction: %d", got)
	}
}
