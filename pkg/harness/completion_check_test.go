package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestCompletionCheckRoutesAndAccountsForVerdicts(t *testing.T) {
	for _, decision := range []string{"gpt-6-luna", ""} {
		t.Run("decision="+decision, func(t *testing.T) {
			var streams, checks, executions, stops int
			client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var req struct {
					Stream bool
					Model  string
					Input  json.RawMessage
					Tools  []struct{ Name string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				contentType, body := "application/json", ""
				if req.Stream {
					streams++
					if req.Model != "main" || len(req.Tools) != 1 || req.Tools[0].Name != "check" {
						t.Fatalf("unexpected main request: %+v", req)
					}
					contentType = "text/event-stream"
					switch streams {
					case 1:
						body = phaseTestResponse(false, strings.ReplaceAll(commentaryOutput, `,"phase":"commentary"`, ""))
					case 2:
						body = phaseTestResponse(false, loopCall("work", "check", "{}"))
					case 3:
						body = phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, ""))
					default:
						t.Fatal("unexpected additional main request")
					}
				} else {
					checks++
					if len(req.Tools) != 0 || !strings.Contains(string(req.Input), "Verify the fixture.") || strings.Contains(string(req.Input), "private context") {
						t.Fatalf("unexpected classifier evidence or tools: %+v", req)
					}
					progress := checks == 1
					usage := `"usage":{"input_tokens":123,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}`
					if decision != "" {
						if req.Model != decision || r.URL.Path != "/v1/decisions" {
							t.Fatalf("decision routed to %s %s", r.URL.Path, req.Model)
						}
						probability := 0.0
						if progress {
							probability = 0.98
						}
						body = fmt.Sprintf(`{"answers":[{"type":"predicate","probability":%g}],%s}`, probability, usage)
					} else {
						if req.Model != "utility" || r.URL.Path != "/v1/responses" {
							t.Fatalf("utility routed to %s %s", r.URL.Path, req.Model)
						}
						body = strings.TrimSuffix(completedReview(fmt.Sprintf(`{"progress":%t}`, progress)), "}") + "," + usage + "}"
					}
				}
				return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}))
			a := &Agent{Config: &Config{
				client: &client, DecisionModel: decision, ConfirmUnphased: true,
				Model: func() string { return "main" }, RoleModel: func(string) (ModelOption, bool) { return ModelOption{ID: "utility"}, true },
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) {
						executions++
						return tool.Text("verified"), nil
					}}}
				},
				Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, answer string, _ bool) (hook.Outcome, error) {
					stops++
					if answer != "Checked and fixed." {
						t.Errorf("Stop received %q", answer)
					}
					return hook.Outcome{}, nil
				}}},
			}}
			if err := a.appendMessages(hiddenContextMessage("private context")); err != nil {
				t.Fatal(err)
			}
			if err := runLoopTurn(t.Context(), a, "Verify the fixture."); err != nil {
				t.Fatal(err)
			}
			if streams != 3 || checks != 2 || executions != 1 || stops != 1 || a.ContextRevision != 0 {
				t.Fatalf("streams=%d checks=%d executions=%d stops=%d context revisions=%d", streams, checks, executions, stops, a.ContextRevision)
			}
			if u := a.UsageSnapshot(); u.InputTokens != 249 || u.OutputTokens != 7 || u.CacheReadInputTokens != 8 || u.LastInputTokens != 1 {
				t.Fatalf("main and classifier accounting: %+v", u)
			}
		})
	}
}

func TestCompletionCheckRequiresUsableVerdict(t *testing.T) {
	for _, tc := range []struct{ route, body string }{
		{"decision", "null"},
		{"decision", `{"answers":[]}`},
		{"decision", `{"answers":[{"type":"predicate"}]}`},
		{"decision", `{"answers":[{"type":"predicate","probability":null}]}`},
		{"decision", `{"answers":[{"type":"predicate","probability":false}]}`},
		{"decision", `{"answers":[{"type":"predicate","probability":"0.9"}]}`},
		{"decision", `{"answers":[{"type":"refusal","message":"declined"}]}`},
		{"decision", `{"answers":[{"type":"predicate","probability":1.1}]}`},
		{"", completedReview(`{}`)},
		{"", "null"},
		{"", completedReview(`{"progress":"yes"}`)},
		{"", filteredReview},
	} {
		a, _ := newReviewTestAgent(t, tc.body)
		a.DecisionModel = tc.route
		progress, err := a.checkProgress(t.Context(), nil, "Maybe.")
		if progress || !errors.Is(err, ErrTurnIncomplete) {
			t.Errorf("body=%s progress=%t err=%v", tc.body, progress, err)
		}
	}
}

func TestCompletionAfterRepeatedProgressAndToolWork(t *testing.T) {
	for _, tc := range []struct {
		name        string
		probability float64
		wantErr     error
	}{
		{"complete", 0.0, nil},
		{"still working", 1.0, ErrMissingFinish},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both earlier progress updates led to work. The third reply must
			// still be checked, even though the recovery budget is exhausted.
			body := fmt.Sprintf(`{"answers":[{"type":"predicate","probability":%g}]}`, tc.probability)
			a, checks := newReviewTestAgent(t, body)
			a.ConfirmUnphased, a.DecisionModel = true, "decision"
			req := &request{tools: confirmationTestTools()}
			s := completionState{unphasedRetries: maxAutomaticContinuations, attempts: 1}
			next, _, err := s.advance(t.Context(), a, req, &response{}, true)
			if err != nil || !next || len(*checks) != 0 {
				t.Fatalf("tool round: next=%t checks=%d err=%v", next, len(*checks), err)
			}
			reply := &response{messages: []Message{{Role: RoleAssistant, Content: []Content{{Text: "A plain reply."}}}}}
			next, notice, err := s.advance(t.Context(), a, req, reply, false)
			if next || notice != nil || len(*checks) != 1 || !errors.Is(err, tc.wantErr) {
				t.Fatalf("last reply: next=%t checks=%d err=%v", next, len(*checks), err)
			}
		})
	}
}
