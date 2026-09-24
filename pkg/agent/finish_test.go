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
		{name: "empty answer argument", first: []string{finishOutput("bad", `{"answer":" "}`)}},
		{name: "non-text answer argument", first: []string{finishOutput("bad", `{"answer":17}`)}},
		{name: "answer cannot bypass work", first: []string{finishOutput("bad", `{"answer":"Premature answer"}`), check}, tools: 1},
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
						if tc.name == "marker without answer" && content.ToolResult.ID == "bad" && !strings.Contains(content.ToolResult.Content, "You have not written a user-facing answer") {
							t.Error("bare marker rejection did not explain the missing answer")
						}
					}
				}
			}
			if len(unresolved) > 0 {
				t.Errorf("unresolved marker calls: %+v", unresolved)
			}
		})
	}
}

func TestExplicitFinishCarriesAnswer(t *testing.T) {
	progress := strings.ReplaceAll(commentaryOutput, `,"phase":"commentary"`, "")
	finish := finishOutput("done", `{"answer":"Checked and fixed."}`)
	for _, tc := range []struct {
		name          string
		responses     []string
		wantPublished int
		wantSaved     string
	}{
		{"answer in marker", []string{phaseTestResponse(false, finish)}, 1, "Checked and fixed."},
		{"same answer is not repeated", []string{phaseTestResponse(false, finalAnswerOutput, finish)}, 0, "Checked and fixed."},
		{"earlier answer is not repeated", []string{phaseTestResponse(false, finalAnswerOutput), phaseTestResponse(false, finish)}, 0, "Checked and fixed."},
		{"cutoff answer is not published", []string{cutoffTestResponse(finishOutput("cut", `{"answer":"Partial answer"}`)), phaseTestResponse(false, finish)}, 1, "Checked and fixed."},
		{"progress cannot hide answer", []string{phaseTestResponse(false, progress, finish)}, 1, "I will check that now.\nChecked and fixed."},
		{"earlier progress cannot hide answer", []string{phaseTestResponse(false, progress), phaseTestResponse(false, finish)}, 1, "I will check that now.\nChecked and fixed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, stops, published := 0, 0, 0
			client := streamingTestClient(func(*http.Request) string {
				if requests >= len(tc.responses) {
					t.Fatal("unexpected model request")
				}
				response := tc.responses[requests]
				requests++
				return response
			})
			a := &Agent{Config: &Config{client: &client, RequireFinish: func(string) bool { return true }, Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, last string, _ bool) (hook.Outcome, error) {
				stops++
				if last != "Checked and fixed." {
					t.Errorf("Stop hook answer=%q", last)
				}
				return hook.Outcome{}, nil
			}}}}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
			if err != nil {
				t.Fatal(err)
			}
			for message, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
				if text := contentText(message.Content); text != "" {
					published++
					if text != "Checked and fixed." || message.Hidden {
						t.Fatalf("published answer=%+v", message)
					}
				}
			}
			if requests != len(tc.responses) || stops != 1 || published != tc.wantPublished {
				t.Fatalf("requests=%d stops=%d published=%d", requests, stops, published)
			}
			data, err := json.Marshal(a.StateSnapshot())
			if err != nil {
				t.Fatal(err)
			}
			var state State
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			var saved []string
			for _, message := range state.Messages {
				if message.Role == RoleAssistant && !message.Hidden && contentText(message.Content) != "" {
					saved = append(saved, contentText(message.Content))
				}
			}
			if strings.Join(saved, "\n") != tc.wantSaved {
				t.Fatalf("saved answers=%q, want %q", saved, tc.wantSaved)
			}
		})
	}
}

// Repeated progress must preserve output without reporting a completed turn.
func TestExplicitFinishInterruptsRepeatedAnswers(t *testing.T) {
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
	if !errors.Is(runErr, ErrTurnIncomplete) || !errors.Is(runErr, ErrMissingFinish) || requests != maxAutomaticContinuations+1 || stops != 0 || a.Running() {
		t.Fatalf("error=%v requests=%d stops=%d running=%t", runErr, requests, stops, a.Running())
	}
	assertIncompleteTurn(t, a)
}

func TestExplicitFinishFailsWithoutAnswer(t *testing.T) {
	requests := 0
	client := streamingTestClient(func(*http.Request) string { requests++; return phaseTestResponse(false) })
	a := &Agent{Config: &Config{client: &client, RequireFinish: func(string) bool { return true }}}
	stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		runErr = errors.Join(runErr, err)
	}
	if !errors.Is(runErr, ErrMissingFinish) || requests != maxAutomaticContinuations+1 || a.Running() {
		t.Fatalf("error=%v requests=%d running=%t", runErr, requests, a.Running())
	}
	assertIncompleteTurn(t, a)
}

func assertIncompleteTurn(t *testing.T, a *Agent) {
	t.Helper()
	state := a.StateSnapshot()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, event := range restored.Events {
		if event.Type == EventTurnTerminal {
			terminals++
			if event.Terminal == nil || event.Terminal.Status != RuntimeIncomplete || event.Terminal.Error == "" {
				t.Fatalf("turn terminal = %+v", event.Terminal)
			}
		}
	}
	if terminals != 1 || len(restored.Messages) != len(state.Messages) {
		t.Fatalf("terminals=%d messages=%d/%d", terminals, len(restored.Messages), len(state.Messages))
	}
}

func TestCompletionLimitProcessesAcceptedSteering(t *testing.T) {
	for _, mode := range []string{"finish", "commentary", "cutoff", "pause"} {
		t.Run(mode, func(t *testing.T) {
			a := &Agent{}
			requests, consumed := 0, 0
			limit := maxAutomaticContinuations + 1
			if mode == "cutoff" {
				limit = 2
			}
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				if requests > limit+1 {
					t.Fatal("unexpected extra request")
				}
				if requests <= limit {
					if mode == "pause" {
						return pausedResponse(commentaryOutput)
					}
					if mode == "cutoff" {
						return cutoffTestResponse(commentaryOutput)
					}
					return phaseTestResponse(false, commentaryOutput)
				}
				var body struct{ Input json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body.Input), "Also verify the new requirement.") {
					t.Fatal("accepted steering missing from continuation")
				}
				if mode == "finish" {
					return phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))
				}
				return phaseTestResponse(false, finalAnswerOutput)
			})
			a.Config = &Config{client: &client, MaxTurns: 10, RequireFinish: func(string) bool { return mode == "finish" }}
			ctx := WithStreamEventHandlers(t.Context(), StreamEventHandlers{Commit: func() {
				if requests == limit && !a.QueueInputWithID([]Content{{Text: "Also verify the new requirement."}}, "late-steer") {
					t.Error("steering rejected while the response was committing")
				}
			}})
			stream, err := a.Send(ctx, []Content{{Text: "Check it."}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, message := range a.MessagesSnapshot() {
				if message.InputID == "late-steer" {
					consumed++
				}
			}
			if requests != limit+1 || consumed != 1 {
				t.Fatalf("requests=%d steering consumed=%d", requests, consumed)
			}
			for _, event := range a.StateSnapshot().Events {
				if event.Type == EventTurnTerminal && event.Terminal.Status != RuntimeCompleted {
					t.Fatalf("terminal = %+v", event.Terminal)
				}
			}
		})
	}
}

// After a reminder, the natural reply to an already written answer is a bare
// marker. Requiring the answer again would show it to the user twice.
func TestExplicitFinishAcceptsBareMarkerAfterAnswer(t *testing.T) {
	bare := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	check := `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`
	for _, tc := range []struct {
		name      string
		responses []string
		accepted  bool
	}{
		{name: "answer then marker", responses: []string{bare, finishOutput("done", "{}")}, accepted: true},
		{name: "empty cutoff preserves answer", responses: []string{bare, cutoffTestResponse(), finishOutput("done", "{}")}, accepted: true},
		{name: "reasoning cutoff preserves answer", responses: []string{bare, cutoffTestResponse(`{"type":"reasoning","id":"rs_cutoff","status":"completed","summary":[{"type":"summary_text","text":"Ready to finish."}]}`), finishOutput("done", "{}")}, accepted: true},
		{name: "partial replacement needs a new answer", responses: []string{bare, cutoffTestResponse(strings.ReplaceAll(bare, "Checked and fixed.", "Actually, ")), finishOutput("bare", "{}"), phaseTestResponse(false, bare, finishOutput("done", "{}"))}},
		{name: "cutoff without answer cannot finish", responses: []string{cutoffTestResponse(), finishOutput("bare", "{}"), phaseTestResponse(false, bare, finishOutput("done", "{}"))}},
		{name: "paused progress needs a new answer", responses: []string{pausedResponse(bare), finishOutput("bare", "{}"), phaseTestResponse(false, bare, finishOutput("done", "{}"))}},
		{name: "work after the answer needs a new answer", responses: []string{bare, check, finishOutput("bare", "{}"), phaseTestResponse(false, bare, finishOutput("done", "{}"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				if requests > len(tc.responses) {
					t.Fatal("unexpected extra model request")
				}
				response := tc.responses[requests-1]
				if strings.HasPrefix(response, "data:") {
					return response
				}
				return phaseTestResponse(false, response)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 10, RequireFinish: func(string) bool { return true },
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Text("ok"), nil }}}
				},
				Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, last string, _ bool) (hook.Outcome, error) {
					if last != "Checked and fixed." {
						t.Errorf("Stop hook lost the answer: %q", last)
					}
					return hook.Outcome{}, nil
				}}},
			}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != len(tc.responses) {
				t.Fatalf("requests = %d, want %d", requests, len(tc.responses))
			}
			var results []string
			for _, message := range a.StateSnapshot().Messages {
				for _, content := range message.Content {
					if content.ToolResult != nil && content.ToolResult.Name == finishToolName {
						results = append(results, content.ToolResult.Content)
					}
				}
			}
			if accepted := len(results) > 0 && results[0] == "Turn finished."; accepted != tc.accepted {
				t.Fatalf("finish results = %q", results)
			}
		})
	}
}

func TestExplicitFinishRequiresFreshAnswerAfterContinuation(t *testing.T) {
	answer := phaseTestResponse(false, finalAnswerOutput)
	marker := phaseTestResponse(false, finishOutput("bare", "{}"))
	done := phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))
	check := `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`
	for _, tc := range []struct {
		name       string
		responses  []string
		steerAfter int
		blockStop  bool
	}{
		{name: "queued input", responses: []string{answer, marker, done}, steerAfter: 1},
		{name: "queued input resets reminders", responses: []string{answer, answer, answer, answer, done}, steerAfter: 2},
		{name: "work in incomplete response", responses: []string{answer, cutoffTestResponse(check), marker, done}},
		{name: "work in incomplete response resets reminders", responses: []string{answer, answer, cutoffTestResponse(check), marker, done}},
		{name: "stop hook after finish", responses: []string{done, marker, done}, blockStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, stops := 0, 0
			a := &Agent{}
			client := streamingTestClient(func(*http.Request) string {
				requests++
				if requests > len(tc.responses) {
					t.Fatal("unexpected extra model request")
				}
				if requests == tc.steerAfter && !a.QueueInput([]Content{{Text: "Also check the new requirement."}}) {
					t.Fatal("steering rejected")
				}
				return tc.responses[requests-1]
			})
			a.Config = &Config{
				client: &client, MaxTurns: 10, RequireFinish: func(string) bool { return true },
				Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Text("ok"), nil }}}
				},
				Hooks: hook.Hooks{Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) {
					stops++
					return hook.Outcome{Block: tc.blockStop && stops == 1, Reason: "Address the remaining requirement."}, nil
				}}},
			}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it."}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != len(tc.responses) {
				t.Fatalf("turn ended after %d requests, want %d", requests, len(tc.responses))
			}
			for _, message := range a.MessagesSnapshot() {
				for _, content := range message.Content {
					if result := content.ToolResult; result != nil && result.ID == "bare" && !result.IsError {
						t.Error("accepted a finish marker without a fresh answer")
					}
				}
			}
		})
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
		{name: "second consecutive cutoff leaves the turn incomplete", responses: []string{cutoff, cutoff}, cutoffs: 1, err: ErrTurnIncomplete},
		{name: "cutoffs do not count as missing finishes", responses: []string{cutoff, answer, cutoff, answer, done}, cutoffs: 2, reminders: 2, stops: 1},
		{name: "repeated answers leave the turn incomplete", responses: []string{cutoff, answer, answer, answer}, cutoffs: 1, reminders: 2, err: ErrMissingFinish},
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
