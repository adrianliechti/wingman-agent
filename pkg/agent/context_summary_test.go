package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestBriefingPreservesPriorCheckpointAndBoundsTranscript(t *testing.T) {
	prior := summaryPrefix + "\n" + strings.Repeat("important prior state. ", 1200) + " STILL_UNFINISHED"
	messages := []Message{hiddenContextMessage(prior), {Role: RoleUser, Content: []Content{{Text: "finish that work"}}}}
	for range 200 {
		messages = append(messages, Message{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("progress ", 600)}}})
	}
	transcript := recoveryTranscript(messages, maxSummarizeBytes)
	if !strings.Contains(transcript, prior) || !strings.Contains(transcript, "finish that work") {
		t.Fatal("checkpoint or user intent lost under transcript pressure")
	}
	if len(transcript) > maxSummarizeBytes {
		t.Fatalf("transcript exceeded its budget: %d", len(transcript))
	}
	for _, budget := range []int{1024, 4096} {
		if got := recoveryTranscript(messages, budget); len(got) > budget || !strings.Contains(got, "omitted") {
			t.Fatalf("bounded recovery input for %d bytes: %d", budget, len(got))
		}
	}
}

func TestBriefingMultipartUserMessageLeavesRoomForEvidence(t *testing.T) {
	message := Message{Role: RoleUser, Content: []Content{
		{Text: "Keep the current task. " + strings.Repeat("first attachment ", 1000)},
		{Text: strings.Repeat("second attachment ", 1000)},
		{Text: strings.Repeat("third attachment ", 1000) + " Preserve the API."},
	}}
	messages := []Message{
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{Content: "RECENT_TEST_RESULT: regression failed; repair pending."}}}},
		message,
	}
	for _, budget := range []int{1024, 4096} {
		transcript := recoveryTranscript(messages, budget)
		if len(transcript) > budget || !strings.Contains(transcript, "RECENT_TEST_RESULT") || !strings.Contains(transcript, "Preserve the API") {
			t.Fatalf("multipart input displaced task evidence at %d bytes: %s", budget, transcript)
		}
	}
}

func TestBriefingOutputCutoffRetriesOnceAndAccountsForBothAttempts(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		t.Run(fmt.Sprint(succeeds), func(t *testing.T) {
			attempts := 0
			var limits []int
			client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var req struct {
					MaxOutputTokens int `json:"max_output_tokens"`
					Reasoning       struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				if req.Reasoning.Effort != "low" {
					t.Fatalf("briefing effort=%q", req.Reasoning.Effort)
				}
				attempts++
				limits = append(limits, req.MaxOutputTokens)
				body := `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":100,"output_tokens":4000}}`
				if attempts == 2 && succeeds {
					body = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"- Test status: not run."}]}],"usage":{"input_tokens":100,"output_tokens":20}}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}))
			a := &Agent{Config: &Config{client: &client, Model: func() string { return "gpt-5.6-terra" }}, Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "run tests"}}}}}
			recap, err := a.Recap(t.Context())
			if attempts != 2 || limits[0] != 4096 || limits[1] != 8192 || (err == nil) != succeeds || (recap != "") != succeeds {
				t.Fatalf("attempts=%d limits=%v recap=%q err=%v", attempts, limits, recap, err)
			}
			if a.UsageSnapshot().InputTokens != 200 || a.UsageSnapshot().LastInputTokens != 0 || a.ContextRevision != 0 {
				t.Fatalf("briefing usage/context=%+v", a.UsageSnapshot())
			}
		})
	}
}

func TestBriefingUsesCurrentHostContext(t *testing.T) {
	a := &Agent{Config: &Config{ContextInstructions: func() string { return "environment=staging" }}}
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	a.ContextInstructions = func() string { return "environment=production" }
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	transcript := recoveryTranscript(a.requestMessages(), maxSummarizeBytes)
	if strings.Contains(transcript, "environment=staging") || !strings.Contains(transcript, "environment=production") || !strings.Contains(transcript, "supplied by the host") {
		t.Fatalf("current metadata lost or mixed with stale context: %s", transcript)
	}
}

func TestRecapCreditsUtilityUsageWithoutChangingContextAnchor(t *testing.T) {
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Task in progress; tests not run."}]}],"usage":{"input_tokens":100,"output_tokens":20}}`))}, nil
	})}))
	a := &Agent{Config: &Config{client: &client, Model: func() string { return "m" }}, Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "task"}}}}, Usage: Usage{InputTokens: 1000, LastInputTokens: 1000}}
	req := &request{model: "m", messages: a.requestMessages()}
	a.anchorContextUsage(req, &response{usage: Usage{InputTokens: 1000}})
	if _, err := a.Recap(context.Background()); err != nil {
		t.Fatal(err)
	}
	usage := a.UsageSnapshot()
	if usage.InputTokens != 1100 || usage.OutputTokens != 20 || usage.LastInputTokens != 1000 {
		t.Fatalf("usage=%+v", usage)
	}
	if current := a.ContextStats().CurrentTokens; current != 1000 {
		t.Fatalf("utility overwrote main context usage: %d", current)
	}
}

func TestBriefingShrinksInputForFailedResponseOverflow(t *testing.T) {
	var inputs []string
	client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, req.Input)
		body := `{"status":"failed","error":{"code":"context_length_exceeded","message":"prompt is too long"}}`
		if len(inputs) == 2 {
			body = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continue the original goal"}]}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}))
	a := &Agent{Config: &Config{client: &client}}
	for range 100 {
		a.Messages = append(a.Messages, Message{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("observed evidence ", 150)}}})
	}
	if _, err := a.Recap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || len(inputs[1]) >= len(inputs[0]) || a.ContextRevision != 0 || len(a.MessagesSnapshot()) != 100 {
		t.Fatalf("overflow retry changed history or repeated input: attempts=%d revision=%d", len(inputs), a.ContextRevision)
	}
}
