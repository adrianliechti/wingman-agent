package agent

import (
	"context"
	"errors"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestTaskBudgetSharedByInlineAgentsWithoutDoubleCharging(t *testing.T) {
	cfg := &Config{MaxTaskTokens: 100, MaxTaskDuration: time.Minute}
	ctx, cancel := cfg.withTaskBudget(t.Context())
	defer cancel()
	child, childCancel := cfg.Derive().withTaskBudget(ctx)
	defer childCancel()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			chargeTaskUsage(child, Usage{InputTokens: 8, OutputTokens: 2, ReasoningTokens: 2, CacheReadInputTokens: 8})
		})
	}
	wg.Wait()
	if !errors.Is(context.Cause(ctx), ErrTaskTokenBudget) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
	if b := ctx.Value(taskBudgetKey{}).(*taskBudget); b.used.Load() != 100 {
		t.Fatalf("charged subsets twice: %d", b.used.Load())
	}
}
func TestTaskTimeoutCancelsQuietProviderAndRecordsOutcome(t *testing.T) {
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}))
	a := &Agent{Config: &Config{client: &client, MaxTaskDuration: 20 * time.Millisecond}}
	stream, err := a.Send(t.Context(), []Content{{Text: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		if err != nil {
			runErr = err
		}
	}
	if !errors.Is(runErr, ErrTaskTimeBudget) {
		t.Fatalf("error = %v", runErr)
	}
	if a.Running() {
		t.Fatal("timed out task still running")
	}
	reviews := a.TurnReviews()
	if len(reviews) != 1 || reviews[0].Outcome != "interrupted" {
		t.Fatalf("outcome = %+v", reviews)
	}
}
func TestTaskBudgetStopsBeforeToolExecution(t *testing.T) {
	requests, executions := 0, 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return strings.ReplaceAll(phaseTestResponse(false, `{"type":"function_call","id":"f","call_id":"c","name":"edit","arguments":"{}","status":"completed"}`), `"input_tokens":1,"output_tokens":1`, `"input_tokens":8,"output_tokens":2`)
	})
	a := &Agent{Config: &Config{client: &client, MaxTaskTokens: 10, Tools: func() []tool.Tool {
		return []tool.Tool{{Name: "edit", Execute: func(context.Context, map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("edited"), nil
		}}}
	}}}
	stream, err := a.Send(t.Context(), []Content{{Text: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		if err != nil {
			runErr = err
		}
	}
	if !errors.Is(runErr, ErrTaskTokenBudget) || requests != 1 || executions != 0 {
		t.Fatalf("requests=%d executions=%d error=%v", requests, executions, runErr)
	}
}
func TestTaskLimitEnvironmentValidation(t *testing.T) {
	for _, value := range []string{"bad", "-1"} {
		t.Setenv("WINGMAN_TASK_MAX_TOKENS", value)
		if (&Config{}).loadTaskLimits() == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	t.Setenv("WINGMAN_TASK_MAX_TOKENS", "1000")
	t.Setenv("WINGMAN_TASK_TIMEOUT", "20m")
	cfg := &Config{}
	if err := cfg.loadTaskLimits(); err != nil {
		t.Fatal(err)
	}
	if derived := cfg.Derive(); derived.MaxTaskTokens != 1000 || derived.MaxTaskDuration != 20*time.Minute {
		t.Fatalf("limits lost: %+v", derived)
	}
}
