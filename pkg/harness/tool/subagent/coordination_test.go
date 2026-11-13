package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestAgentWaitAllAndAny(t *testing.T) {
	registry := task.NewRegistry()
	t.Cleanup(registry.Close)

	slowGate := make(chan struct{})
	fastTask, err := registry.Launch("fast audit", "explore", func(_ context.Context, _ *task.Task) (string, error) {
		time.Sleep(15 * time.Millisecond)
		return "fast result ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slowTask, err := registry.Launch("slow audit", "explore", func(ctx context.Context, _ *task.Task) (string, error) {
		select {
		case <-slowGate:
			return "slow result ok", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	var waitTool tool.Tool
	for _, tl := range taskTools(registry) {
		if tl.Name == "agent_wait" {
			waitTool = tl
		}
	}

	// mode: "any" returns as soon as fastTask finishes while slowTask is still running.
	anyRes, err := waitTool.Execute(t.Context(), map[string]any{
		"ids":             []any{fastTask.ID, slowTask.ID},
		"mode":            "any",
		"timeout_seconds": 5,
	})
	if err != nil {
		t.Fatalf("agent_wait mode=any failed: %v", err)
	}
	if !strings.Contains(anyRes.Content.Text(), "fast result ok") || !strings.Contains(anyRes.Content.Text(), "still running") {
		t.Fatalf("unexpected mode=any output:\n%s", anyRes.Content.Text())
	}

	close(slowGate)
	// mode: "all" waits for both and returns both results.
	allRes, err := waitTool.Execute(t.Context(), map[string]any{
		"ids":             []any{fastTask.ID, slowTask.ID},
		"mode":            "all",
		"timeout_seconds": 5,
	})
	if err != nil {
		t.Fatalf("agent_wait mode=all failed: %v", err)
	}
	if !strings.Contains(allRes.Content.Text(), "fast result ok") || !strings.Contains(allRes.Content.Text(), "slow result ok") {
		t.Fatalf("unexpected mode=all output:\n%s", allRes.Content.Text())
	}
}

func TestSubagentForkContextSeedsCompletedParentHistory(t *testing.T) {
	var capturedBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capturedBody.CompareAndSwap(nil, string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": reviewText("forked child done")}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	t.Cleanup(server.Close)
	t.Setenv("WINGMAN_URL", server.URL)
	cfg, err := harness.ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}

	parentHistory := []harness.Message{
		{Role: harness.RoleUser, Content: []harness.Content{{Text: "Earlier architecture context: use hexagonal ports"}}},
		{Role: harness.RoleAssistant, Content: []harness.Content{
			{Text: "Understood, delegating."},
			// The unanswered in-flight call from the current parent step must be stripped.
			{ToolCall: &harness.ToolCall{ID: "inflight_agent_call", Name: "agent", Args: "{}"}},
		}},
	}
	tools := ToolsWithOptions(cfg, Options{ParentMessages: func() []harness.Message { return parentHistory }})

	for _, fork := range []bool{true, false} {
		capturedBody = atomic.Value{}
		res, err := tools[0].Execute(t.Context(), map[string]any{
			"description": "follow-up audit", "prompt": "audit the ports", "agent_type": "general-purpose", "fork_context": fork,
		})
		if err != nil {
			t.Fatalf("fork_context=%v: %v", fork, err)
		}
		body, _ := capturedBody.Load().(string)
		if got := strings.Contains(body, "Earlier architecture context: use hexagonal ports"); got != fork {
			t.Fatalf("fork_context=%v: parent history present=%v:\n%s", fork, got, body)
		}
		if strings.Contains(body, "inflight_agent_call") {
			t.Fatalf("fork_context=%v leaked the unanswered parent tool call:\n%s", fork, body)
		}
		if !strings.Contains(res.Content.Text(), "forked child done") {
			t.Fatalf("fork_context=%v result = %q", fork, res.Content.Text())
		}
	}
}

// Children cannot launch agents, so they must not be handed the tools that
// manage them either.
func TestChildrenDoNotReceiveAgentCoordinationTools(t *testing.T) {
	registry := task.NewRegistry()
	t.Cleanup(registry.Close)
	for _, tl := range taskTools(registry) {
		if allowNonAgentTool(tl) || allowReadOnlyTool(tl) {
			t.Errorf("%s is exposed to subagents", tl.Name)
		}
	}
}
