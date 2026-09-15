package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func finishOutput(id, args string) string {
	b, _ := json.Marshal(map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": finishToolName, "arguments": args, "status": "completed"})
	return string(b)
}

func TestExplicitFinish(t *testing.T) {
	bare := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	check := `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`
	for _, tc := range []struct {
		name  string
		first []string
		tools int
	}{
		{name: "unmarked answer", first: []string{bare}},
		{name: "upstream invented final phase", first: []string{finalAnswerOutput}},
		{name: "marker with work", first: []string{bare, finishOutput("bad", "{}"), check}, tools: 1},
		{name: "marker without answer", first: []string{finishOutput("bad", "{}")}},
		{name: "invalid marker arguments", first: []string{bare, finishOutput("bad", `{"unexpected":true}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, tools, stops := 0, 0, 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				var body struct {
					Instructions string
					Tools        []struct{ Name string }
					Input        []struct {
						Type   string
						CallID string `json:"call_id"`
						Output string
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, t := range body.Tools {
					found = found || t.Name == finishToolName
				}
				if !found {
					t.Error("finish tool not offered")
				}
				if !strings.Contains(body.Instructions, finishInstructions) || strings.HasPrefix(body.Instructions, "\n") {
					t.Errorf("instructions = %q", body.Instructions)
				}
				// Every replayed output must follow its call so strict backends
				// accept the history, including rejected finish markers.
				called := map[string]bool{}
				for _, item := range body.Input {
					switch item.Type {
					case "function_call":
						called[item.CallID] = true
					case "function_call_output":
						if !called[item.CallID] {
							t.Errorf("output for %q precedes its call", item.CallID)
						}
					}
				}
				if stops != 0 {
					t.Error("stop hook ran before explicit finish")
				}
				if requests == 1 {
					return phaseTestResponse(false, tc.first...)
				}
				return phaseTestResponse(false, bare, finishOutput("done", "{}"))
			})
			a := &Agent{Config: &Config{
				client: &client, MaxTurns: 3, RequireFinish: func(string) bool { return true },
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) { tools++; return tool.Text("ok"), nil }}}
				},
				Hooks: hook.Hooks{Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) { stops++; return hook.Outcome{}, nil }}},
			}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
			if err != nil {
				t.Fatal(err)
			}
			for message, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
				for _, call := range extractToolCalls([]Message{message}) {
					if call.Name == finishToolName {
						t.Error("internal marker leaked into visible output")
					}
				}
			}
			if requests != 2 || tools != tc.tools || stops != 1 {
				t.Fatalf("requests/tools/stops = %d/%d/%d", requests, tools, stops)
			}
			state := a.StateSnapshot()
			unresolved := map[string]bool{}
			for _, message := range state.Messages {
				for _, call := range extractToolCalls([]Message{message}) {
					unresolved[call.ID] = true
					if call.Name == finishToolName && !message.Hidden {
						t.Error("finish marker visible in saved transcript")
					}
				}
				for _, content := range message.Content {
					if content.ToolResult != nil {
						delete(unresolved, content.ToolResult.ID)
					}
				}
			}
			if len(unresolved) > 0 {
				t.Errorf("unresolved marker calls: %+v", unresolved)
			}
		})
	}
}

func TestExplicitFinishIsBounded(t *testing.T) {
	requests, stops := 0, 0
	client := streamingTestClient(func(*http.Request) string { requests++; return phaseTestResponse(false, commentaryOutput) })
	a := &Agent{Config: &Config{client: &client, RequireFinish: func(string) bool { return true }, Hooks: hook.Hooks{Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) { stops++; return hook.Outcome{}, nil }}}}}
	stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		runErr = errors.Join(runErr, err)
	}
	if !errors.Is(runErr, ErrMissingFinish) || requests != maxFinishReminders+1 || stops != 0 || a.Running() {
		t.Fatalf("error=%v requests=%d stops=%d running=%t", runErr, requests, stops, a.Running())
	}
}

func TestExplicitFinishPreservesStopHookContinuation(t *testing.T) {
	requests, stops := 0, 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return phaseTestResponse(false, finalAnswerOutput, finishOutput(fmt.Sprintf("done_%d", requests), "{}"))
	})
	a := &Agent{Config: &Config{client: &client, MaxTurns: 3, RequireFinish: func(string) bool { return true }, Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, _ string, active bool) (hook.Outcome, error) {
		stops++
		if stops == 1 {
			return hook.Outcome{Block: true, Reason: "Verify again."}, nil
		}
		if !active {
			t.Error("lost stop-hook state")
		}
		return hook.Outcome{}, nil
	}}}}}
	stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests != 2 || stops != 2 {
		t.Fatalf("requests=%d stops=%d", requests, stops)
	}
}

func TestExplicitFinishExceptions(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprintf("structured=%t", structured), func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				if structured {
					var body struct{ Tools []any }
					json.NewDecoder(r.Body).Decode(&body)
					if len(body.Tools) != 0 {
						t.Error("structured finalization offered tools")
					}
					return phaseTestResponse(false, finalAnswerOutput)
				}
				return phaseTestResponse(false, `{"type":"message","id":"refusal","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"Cannot answer"}]}`)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 2, RequireFinish: func(string) bool { return true }}}
			ctx := t.Context()
			if structured {
				ctx = WithOutputSchema(ctx, map[string]any{})
			}
			stream, err := a.Send(ctx, []Content{{Text: "Answer."}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}

func cutoffTestResponse(items ...string) string {
	stream := strings.ReplaceAll(phaseTestResponse(false, items...), "response.completed", "response.incomplete")
	return strings.ReplaceAll(stream, `"response":{`, `"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},`)
}

// A cutoff is a transport boundary and gets the cutoff notice, never a finish
// reminder, so long outputs cannot exhaust the reminder budget.
func TestExplicitFinishCutoff(t *testing.T) {
	bare := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	cutoff := cutoffTestResponse(bare)
	answer := phaseTestResponse(false, bare)
	done := phaseTestResponse(false, bare, finishOutput("done", "{}"))
	for _, tc := range []struct {
		name               string
		responses          []string
		cutoffs, reminders int
		stops              int
		err                error
	}{
		{name: "cutoff then finish", responses: []string{cutoff, done}, cutoffs: 1, stops: 1},
		{name: "second consecutive cutoff ends the turn", responses: []string{cutoff, cutoff, cutoff}, cutoffs: 1},
		{name: "cutoffs do not count as missing finishes", responses: []string{cutoff, answer, cutoff, answer, done}, cutoffs: 2, reminders: 2, stops: 1},
		{name: "reminder budget still bounds unfinished answers", responses: []string{cutoff, answer, answer, answer}, cutoffs: 1, reminders: 2, err: ErrMissingFinish},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, stops := 0, 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				if requests > len(tc.responses) {
					t.Fatalf("unexpected request %d", requests)
				}
				return tc.responses[requests-1]
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 10, RequireFinish: func(string) bool { return true },
				Hooks: hook.Hooks{Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) { stops++; return hook.Outcome{}, nil }}}}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Write it."}})
			if err != nil {
				t.Fatal(err)
			}
			var runErr error
			for _, err := range stream {
				runErr = errors.Join(runErr, err)
			}
			if !errors.Is(runErr, tc.err) || (tc.err == nil && runErr != nil) {
				t.Fatalf("error=%v, want %v", runErr, tc.err)
			}
			wantRequests := len(tc.responses)
			if tc.name == "second consecutive cutoff ends the turn" {
				wantRequests = 2
			}
			if requests != wantRequests || stops != tc.stops {
				t.Fatalf("requests=%d stops=%d, want %d/%d", requests, stops, wantRequests, tc.stops)
			}
			cutoffs, reminders := 0, 0
			for _, message := range a.StateSnapshot().Messages {
				if !message.Hidden || message.Role != RoleUser {
					continue
				}
				switch text := contentText(message.Content); {
				case text == finishReminder:
					reminders++
				case strings.Contains(text, "cut off"):
					cutoffs++
				}
			}
			if cutoffs != tc.cutoffs || reminders != tc.reminders {
				t.Fatalf("cutoff notices=%d finish reminders=%d, want %d/%d", cutoffs, reminders, tc.cutoffs, tc.reminders)
			}
		})
	}
}
