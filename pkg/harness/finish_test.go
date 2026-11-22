package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func finishOutput(id, args string) string {
	b, _ := json.Marshal(map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": finishToolName, "arguments": args, "status": "completed"})
	return string(b)
}

func confirmationTestTools() []tool.Tool {
	return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Text("ok"), nil
	}}}
}

func TestResumeWithLegacyFinishReminders(t *testing.T) {
	requests := 0
	client := streamingTestClient(func(r *http.Request) string {
		requests++
		req := readLoopRequest(t, r)
		input := strings.Join(loopInputOrder(req.Input), "\n")
		if strings.Contains(input, finishToolName) || !strings.Contains(input, continuationReminder) {
			t.Fatalf("stale finish instructions in resumed request: %s", input)
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})
	a := &Agent{Config: &Config{client: &client, ConfirmUnphased: true, Tools: confirmationTestTools}}
	if err := a.Restore(State{Messages: []Message{hiddenContextMessage(finishReminder), hiddenContextMessage(legacyMissingAnswerReminder), hiddenContextMessage(missingAnswerReminder)}}); err != nil {
		t.Fatal(err)
	}
	if err := runLoopTurn(t.Context(), a, "Continue."); err != nil || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}

func TestFinishToolHandling(t *testing.T) {
	t.Run("consumer stop during tool drain still rejects committed finish_turn", func(t *testing.T) {
		check := `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`
		finish := finishOutput("premature", "{}")
		client := streamingTestClient(func(*http.Request) string {
			return itemDoneEvents(check, finish) + phaseTestResponse(false, check, finish)
		})
		commits := 0
		a := &Agent{Config: &Config{
			client:          &client,
			ConfirmUnphased: true,
			Tools: func() []tool.Tool {
				return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) {
					return tool.Text("checked"), nil
				}}}
			},
		}}
		ctx := WithStreamEventHandlers(t.Context(), StreamEventHandlers{Commit: func() { commits++ }})
		stream, err := a.Send(ctx, []Content{{Text: "Run check."}})
		if err != nil {
			t.Fatal(err)
		}
		for msg, err := range stream {
			if err != nil {
				t.Fatal(err)
			}
			if len(msg.Content) > 0 && msg.Content[0].ToolResult != nil && msg.Content[0].ToolResult.ID == "check" {
				break
			}
		}
		if commits != 2 {
			t.Fatalf("commits = %d, want 2 (2 output items committed before drain)", commits)
		}
		foundFinishResult := false
		for _, m := range a.MessagesSnapshot() {
			for _, c := range m.Content {
				if c.ToolResult != nil && c.ToolResult.ID == "premature" && c.ToolResult.IsError {
					foundFinishResult = true
				}
			}
		}
		if !foundFinishResult {
			t.Fatal("committed finish_turn was left without a rejected ToolResult after consumer stopped on tool drain")
		}
	})

	t.Run("user tool named finish_turn is reserved during automatic detection", func(t *testing.T) {
		executed := 0
		a := &Agent{Config: &Config{
			Model:           func() string { return "gpt-6-sol" },
			ConfirmUnphased: true,
			Tools: func() []tool.Tool {
				return []tool.Tool{{Name: finishToolName, Execute: func(context.Context, map[string]any) (tool.Result, error) {
					executed++
					return tool.Text("ran"), nil
				}}}
			},
		}}
		stream, err := a.Send(t.Context(), []Content{{Text: "Run."}})
		if err != nil {
			t.Fatal(err)
		}
		var runErr error
		for _, err := range stream {
			runErr = errors.Join(runErr, err)
		}
		if runErr == nil || !strings.Contains(runErr.Error(), "reserved for turn confirmation") || executed != 0 {
			t.Fatalf("err=%v executed=%d, want reserved tool error and 0 executions", runErr, executed)
		}
	})
}

func TestRejectedOptionalFinishWithExplicitTurnBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		endTurn      string
		args         string
		repeat       bool
		wantRequests int
	}{
		{"end turn then recover", "true", "{}", false, 2},
		{"continue then recover", "false", "{}", false, 2},
		{"answer in unadvertised marker cannot override continuation", "false", `{"answer":"Draft."}`, false, 2},
		{"repeated end turn is bounded", "true", "{}", true, 3},
		{"repeated continuation is bounded", "false", "{}", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				if requests > 1 {
					var body struct{ Input json.RawMessage }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(body.Input), "Finish rejected.") {
						t.Error("follow-up did not receive the rejected finish result")
					}
				}
				if requests == 1 || tc.repeat {
					return endTurnResponse(tc.endTurn, finishOutput(fmt.Sprintf("bad_%d", requests), tc.args))
				}
				return endTurnResponse("true", finalAnswerOutput)
			})
			a := &Agent{Config: &Config{
				client: &client, MaxTurns: 5,
				ConfirmUnphased: true, Tools: confirmationTestTools,
			}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Answer."}})
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			var runErr error
			for message, err := range stream {
				runErr = errors.Join(runErr, err)
				visible.WriteString(contentText(message.Content))
			}
			if requests != tc.wantRequests {
				t.Errorf("requests = %d, want %d", requests, tc.wantRequests)
			}
			if tc.repeat {
				if !errors.Is(runErr, ErrTurnIncomplete) {
					t.Fatalf("error = %v, want ErrTurnIncomplete", runErr)
				}
				assertIncompleteTurn(t, a)
			} else {
				if runErr != nil {
					t.Fatal(runErr)
				}
				if got := visible.String(); got != "Checked and fixed." {
					t.Errorf("visible answer = %q, want Checked and fixed.", got)
				}
				if outcome := a.TurnReviews()[0].Outcome; outcome != "completed" {
					t.Errorf("outcome = %q, want completed", outcome)
				}
			}
		})
	}
}

func TestConfirmationMarker(t *testing.T) {
	bare := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	check := `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`
	for _, tc := range []struct {
		name  string
		first []string
		tools int
	}{
		{name: "unmarked answer", first: []string{bare}},
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
				if found != (requests == 2 || requests == 3 && tc.tools == 0) {
					t.Errorf("request %d offered finish_turn=%t", requests, found)
				}
				if strings.Contains(body.Instructions, finishInstructions) != found || strings.HasPrefix(body.Instructions, "\n") {
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
					return phaseTestResponse(false, bare)
				}
				if requests == 2 {
					return phaseTestResponse(false, tc.first...)
				}
				return phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))
			})
			a := &Agent{Config: &Config{
				client: &client, MaxTurns: 3, ConfirmUnphased: true,
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
			if requests != 3 || tools != tc.tools || stops != 1 {
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
						if tc.name == "marker without answer" && content.ToolResult.ID == "bad" && !strings.Contains(content.ToolResult.Content.Text(), "requires an explicit final_answer") {
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

func TestConfirmUnphasedStops(t *testing.T) {
	unphased := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	progress := phaseTestResponse(false, strings.ReplaceAll(commentaryOutput, `,"phase":"commentary"`, ""))
	final := phaseTestResponse(false, finalAnswerOutput)
	plain := phaseTestResponse(false, unphased)
	check := phaseTestResponse(false, loopCall("work", "check", "{}"))
	marker := endTurnResponse("false", finishOutput("done", `{"answer":"Checked and fixed."}`))
	type step struct {
		reply        string
		offersFinish bool
	}
	for _, tc := range []struct {
		name            string
		steps           []step
		noTools, schema bool
		work            int
		wantErr         error
	}{
		{name: "explicit final", steps: []step{{final, false}}},
		{name: "native end", steps: []step{{endTurnResponse("true", unphased), false}}},
		{name: "native stop reason", steps: []step{{strings.Replace(plain, `"response":{`, `"response":{"stop_reason":"end_turn",`, 1), false}}},
		{name: "explicit continuation overrides stop reason", steps: []step{{strings.Replace(plain, `"response":{`, `"response":{"stop_reason":"end_turn","end_turn":false,`, 1), false}, {final, false}}},
		{name: "native continuation", steps: []step{{endTurnResponse("false", unphased), false}, {final, false}}},
		{name: "commentary", steps: []step{{phaseTestResponse(false, commentaryOutput), false}, {final, false}}},
		{name: "no tools", steps: []step{{plain, false}}, noTools: true},
		{name: "structured output", steps: []step{{plain, false}}, schema: true},
		{name: "genuine unphased final", steps: []step{{plain, false}, {marker, true}}},
		{name: "empty stop", steps: []step{{phaseTestResponse(false), false}, {check, true}, {final, false}}, work: 1},
		{name: "native final during confirmation", steps: []step{{progress, false}, {final, true}}},
		{name: "native end during confirmation", steps: []step{{progress, false}, {endTurnResponse("true", unphased), true}}},
		{name: "resume real work", steps: []step{{progress, false}, {endTurnResponse("false", finishOutput("bare", "{}")), true}, {check, true}, {plain, false}, {marker, true}}, work: 1},
		{name: "bounded", steps: []step{{progress, false}, {progress, true}, {progress, true}}, wantErr: ErrMissingFinish},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, executions := 0, 0
			client := streamingTestClient(func(r *http.Request) string {
				if requests >= len(tc.steps) {
					t.Fatal("unexpected extra model request")
				}
				step := tc.steps[requests]
				requests++
				var names []string
				for _, tool := range readLoopRequest(t, r).Tools {
					names = append(names, tool.Name)
				}
				if slices.Contains(names, finishToolName) != step.offersFinish || slices.Contains(names, "check") != (!tc.noTools && !tc.schema) {
					t.Fatalf("request %d tools=%v, want finish=%t", requests, names, step.offersFinish)
				}
				return step.reply
			})
			cfg := &Config{client: &client, ConfirmUnphased: true}
			if !tc.noTools {
				cfg.Tools = func() []tool.Tool {
					return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) {
						executions++
						return tool.Text("verified"), nil
					}}}
				}
			}
			ctx := t.Context()
			if tc.schema {
				ctx = WithOutputSchema(ctx, map[string]any{"type": "object"})
			}
			a := &Agent{Config: cfg}
			err := runLoopTurn(ctx, a, "Check it.")
			if !errors.Is(err, tc.wantErr) || requests != len(tc.steps) || executions != tc.work {
				t.Fatalf("error=%v requests=%d work=%d", err, requests, executions)
			}
			if a.ContextRevision != 0 {
				t.Errorf("automatic confirmation rewrote the conversation %d times", a.ContextRevision)
			}
			if tc.wantErr == nil && lastAssistantText(a.MessagesSnapshot()) != "Checked and fixed." {
				t.Fatalf("final answer = %q", lastAssistantText(a.MessagesSnapshot()))
			}
		})
	}
}

func TestFinishCarriesAnswer(t *testing.T) {
	progress := strings.ReplaceAll(commentaryOutput, `,"phase":"commentary"`, "")
	finish := finishOutput("done", `{"answer":"Checked and fixed."}`)
	for _, tc := range []struct {
		name        string
		responses   []string
		wantVisible []string
	}{
		{"answer in marker", []string{phaseTestResponse(false, finish)}, []string{"Checked and fixed."}},
		{"same answer is not repeated", []string{phaseTestResponse(false, finalAnswerOutput, finish)}, []string{"Checked and fixed."}},
		{"unphased answer confirmed without duplication", []string{endTurnResponse("null", strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")), endTurnResponse("false", finish)}, []string{"Checked and fixed."}},
		{"earlier answer is not repeated", []string{endTurnResponse("false", finalAnswerOutput), phaseTestResponse(false, finish)}, []string{"Checked and fixed."}},
		{"literal empty JSON object", []string{phaseTestResponse(false, finishOutput("done", `{"answer":"{}"}`))}, []string{"{}"}},
		{"literal answer after earlier text", []string{endTurnResponse("false", finalAnswerOutput), phaseTestResponse(false, finishOutput("done", `{"answer":"{}"}`))}, []string{"Checked and fixed.", "{}"}},
		{"cutoff answer is not published", []string{cutoffTestResponse(finishOutput("cut", `{"answer":"Partial answer"}`)), phaseTestResponse(false, finish)}, []string{"Checked and fixed."}},
		{"progress cannot hide answer", []string{phaseTestResponse(false, progress, finish)}, []string{"I will check that now.", "Checked and fixed."}},
		{"earlier progress cannot hide answer", []string{phaseTestResponse(false, progress), phaseTestResponse(false, finish)}, []string{"I will check that now.", "Checked and fixed."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, stops := 0, 0
			var visible []string
			client := streamingTestClient(func(*http.Request) string {
				if requests >= len(tc.responses) {
					t.Fatal("unexpected model request")
				}
				response := tc.responses[requests]
				requests++
				return response
			})
			a := &Agent{Config: &Config{client: &client, ConfirmUnphased: true, Tools: confirmationTestTools, Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, last string, _ bool) (hook.Outcome, error) {
				stops++
				if last != tc.wantVisible[len(tc.wantVisible)-1] {
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
					visible = append(visible, text)
					if message.Hidden {
						t.Fatalf("published answer=%+v", message)
					}
				}
			}
			if !slices.Equal(visible, tc.wantVisible) {
				t.Fatalf("visible answer=%q, want %q", visible, tc.wantVisible)
			}
			if requests != len(tc.responses) || stops != 1 {
				t.Fatalf("requests=%d stops=%d", requests, stops)
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
			if !slices.Equal(saved, tc.wantVisible) {
				t.Fatalf("saved answers=%q, want %q", saved, tc.wantVisible)
			}
		})
	}
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
					if mode == "finish" {
						return phaseTestResponse(false)
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
			a.Config = &Config{client: &client, MaxTurns: 10, ConfirmUnphased: mode == "finish", Tools: confirmationTestTools}
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

// A native continuation can preserve a final answer for a later bare marker.
func TestFinishAcceptsBareMarkerAfterAnswer(t *testing.T) {
	bare := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	check := `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`
	for _, tc := range []struct {
		name      string
		responses []string
		accepted  bool
	}{
		{name: "answer then marker", responses: []string{endTurnResponse("false", finalAnswerOutput), finishOutput("done", "{}")}, accepted: true},
		{name: "malformed marker preserves answer", responses: []string{phaseTestResponse(false, finalAnswerOutput, finishOutput("bad", `{"unexpected":true}`)), finishOutput("done", "{}")}},
		{name: "partial answer cannot finish", responses: []string{strings.ReplaceAll(finalAnswerOutput, "final_answer", "partial_answer"), finishOutput("bare", "{}"), phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))}},
		{name: "partial replacement cannot reuse prior answer", responses: []string{endTurnResponse("false", finalAnswerOutput), phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "final_answer", "partial_answer"), finishOutput("bare", "{}")), phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))}},
		{name: "empty cutoff preserves answer", responses: []string{endTurnResponse("false", finalAnswerOutput), cutoffTestResponse(), finishOutput("done", "{}")}, accepted: true},
		{name: "reasoning cutoff preserves answer", responses: []string{endTurnResponse("false", finalAnswerOutput), cutoffTestResponse(`{"type":"reasoning","id":"rs_cutoff","status":"completed","summary":[{"type":"summary_text","text":"Ready to finish."}]}`), finishOutput("done", "{}")}, accepted: true},
		{name: "partial replacement needs a new answer", responses: []string{endTurnResponse("false", finalAnswerOutput), cutoffTestResponse(strings.ReplaceAll(bare, "Checked and fixed.", "Actually, ")), finishOutput("bare", "{}"), phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))}},
		{name: "cutoff without answer cannot finish", responses: []string{cutoffTestResponse(), finishOutput("bare", "{}"), phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))}},
		{name: "paused progress needs a new answer", responses: []string{pausedResponse(bare), finishOutput("bare", "{}"), phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))}},
		{name: "work after the answer needs a new answer", responses: []string{endTurnResponse("false", finalAnswerOutput), check, finishOutput("bare", "{}"), phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))}},
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
			a := &Agent{Config: &Config{client: &client, MaxTurns: 10, ConfirmUnphased: true,
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
						results = append(results, content.ToolResult.Content.Text())
					}
				}
			}
			if accepted := len(results) > 0 && results[0] == "Turn finished."; accepted != tc.accepted {
				t.Fatalf("finish results = %q", results)
			}
		})
	}
}

func TestFinishRequiresFreshAnswerAfterContinuation(t *testing.T) {
	answer := endTurnResponse("false", finalAnswerOutput)
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
				client: &client, MaxTurns: 10, ConfirmUnphased: true,
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

func TestFinishPreservesStopHookContinuation(t *testing.T) {
	requests, stops := 0, 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return phaseTestResponse(false, finalAnswerOutput, finishOutput(fmt.Sprintf("done_%d", requests), "{}"))
	})
	a := &Agent{Config: &Config{client: &client, MaxTurns: 3, ConfirmUnphased: true, Tools: confirmationTestTools, Hooks: hook.Hooks{Stop: []hook.Stop{func(_ context.Context, _ string, active bool) (hook.Outcome, error) {
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

func TestFinishExceptions(t *testing.T) {
	for _, mode := range []string{"schema", "refusal item", "refusal delta", "refusal stop"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(r *http.Request) string {
				requests++
				if mode == "schema" {
					var body struct{ Tools []any }
					json.NewDecoder(r.Body).Decode(&body)
					if len(body.Tools) != 0 {
						t.Error("structured finalization offered tools")
					}
					return phaseTestResponse(false, finalAnswerOutput)
				}
				if mode == "refusal stop" {
					return strings.Replace(endTurnResponse("true"), `"end_turn":true`, `"end_turn":true,"stop_reason":"refusal"`, 1)
				}
				if mode == "refusal delta" {
					return `data: {"type":"response.refusal.delta","item_id":"refusal","delta":"Cannot answer"}` + "\n\n" + phaseTestResponse(false)
				}
				return phaseTestResponse(false, `{"type":"message","id":"refusal","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"Cannot answer"}]}`)
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 2, ConfirmUnphased: true, Tools: confirmationTestTools}}
			ctx := t.Context()
			if mode == "schema" {
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
func TestFinishCutoff(t *testing.T) {
	bare := strings.ReplaceAll(finalAnswerOutput, `,"phase":"final_answer"`, "")
	cutoff := cutoffTestResponse(bare)
	answer := phaseTestResponse(false, bare)
	done := phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))
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
			a := &Agent{Config: &Config{client: &client, MaxTurns: 10, ConfirmUnphased: true, Tools: confirmationTestTools,
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
				case text == continuationReminder:
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
