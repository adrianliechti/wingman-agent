package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func streamingTestClient(body func(*http.Request) string) openai.Client {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body(r))),
			Request:    r,
		}, nil
	})}

	return openai.NewClient(
		option.WithBaseURL("http://agent.test"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(httpClient),
	)
}

func TestSendLimitsRunawayToolCallRounds(t *testing.T) {
	var requests atomic.Int64

	client := streamingTestClient(func(*http.Request) string {
		request := requests.Add(1)
		return fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_%d\",\"call_id\":\"call_%d\",\"name\":\"loop\",\"arguments\":\"{}\",\"status\":\"completed\"}}\n\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"usage\":{\"input_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":1}}}\n\ndata: [DONE]\n\n", request, request)
	})

	var executions atomic.Int64
	a := &Agent{Config: &Config{
		client:   &client,
		MaxTurns: 3,
		Tools: func() []tool.Tool {
			return []tool.Tool{{
				Name: "loop",
				Execute: func(context.Context, map[string]any) (string, error) {
					executions.Add(1)
					return "again", nil
				},
			}}
		},
	}}

	var runErr error
	for _, err := range a.Send(context.Background(), []Content{{Text: "start"}}) {
		if err != nil {
			runErr = err
		}
	}

	if !errors.Is(runErr, ErrMaxTurnsExceeded) {
		t.Fatalf("run error = %v, want %v", runErr, ErrMaxTurnsExceeded)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("model requests = %d, want 3", got)
	}
	if got := executions.Load(); got != 3 {
		t.Fatalf("tool executions = %d, want 3", got)
	}
}

func TestEndRunPreservesQueuedUserInput(t *testing.T) {
	a := &Agent{
		Config:       &Config{},
		running:      true,
		pendingInput: [][]Content{{{Text: "queued"}}},
	}

	a.endRun()

	if a.running || len(a.pendingInput) != 0 {
		t.Fatalf("run state was not cleared: running=%v pending=%d", a.running, len(a.pendingInput))
	}
	if len(a.Messages) != 1 || a.Messages[0].Role != RoleUser || a.Messages[0].Content[0].Text != "queued" {
		t.Fatalf("queued input was not preserved: %+v", a.Messages)
	}
}

func TestCompleteBackfillsOutputFromTerminalEvent(t *testing.T) {
	client := streamingTestClient(func(*http.Request) string {
		return "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"
	})

	resp, err := complete(context.Background(), &client, &request{}, yieldAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.messages) != 1 || len(resp.messages[0].Content) != 1 || resp.messages[0].Content[0].Text != "done" {
		t.Fatalf("terminal output was not backfilled: %+v", resp.messages)
	}
}

func TestCompleteRejectsStreamWithoutTerminalEvent(t *testing.T) {
	client := streamingTestClient(func(*http.Request) string {
		return "data: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{}}\n\ndata: [DONE]\n\n"
	})

	_, err := complete(context.Background(), &client, &request{}, yieldAll)
	if err == nil {
		t.Fatal("expected an incomplete stream error")
	}
	if !isRecoverableError(err) {
		t.Fatalf("pre-output incomplete stream should be retryable: %v", err)
	}
}

func TestCompleteDoesNotRetryStreamWithoutTerminalAfterOutput(t *testing.T) {
	client := streamingTestClient(func(*http.Request) string {
		return "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\ndata: [DONE]\n\n"
	})

	_, err := complete(context.Background(), &client, &request{}, yieldAll)
	if err == nil {
		t.Fatal("expected an incomplete stream error")
	}
	if isRecoverableError(err) {
		t.Fatalf("post-output incomplete stream must not be replayed: %v", err)
	}
}
