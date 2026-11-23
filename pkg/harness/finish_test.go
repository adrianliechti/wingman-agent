package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

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
		if strings.Contains(input, "legacy-marker") || strings.Contains(input, "finish_turn") || !strings.Contains(input, "Earlier answer.") || !strings.Contains(input, continuationReminder) {
			t.Fatalf("stale finish instructions in resumed request: %s", input)
		}
		return phaseTestResponse(false, finalAnswerOutput)
	})
	a := &Agent{Config: &Config{client: &client, ConfirmUnphased: true, Tools: confirmationTestTools}}
	if err := a.Restore(State{Messages: []Message{
		hiddenContextMessage(confirmationReminder), hiddenContextMessage(finishReminder), hiddenContextMessage(legacyMissingAnswerReminder), hiddenContextMessage(missingAnswerReminder),
		{Role: RoleAssistant, Hidden: true, Content: []Content{{ToolCall: &ToolCall{Name: "finish_turn", ID: "legacy-marker", Args: "{}"}}}},
		{Role: RoleAssistant, Hidden: true, Content: []Content{{ToolResult: &ToolResult{Name: "finish_turn", ID: "legacy-marker", Content: tool.TextContent("Turn finished.")}}}},
		{Role: RoleAssistant, Content: []Content{{Text: "Earlier answer."}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := runLoopTurn(t.Context(), a, "Continue."); err != nil || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
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
	for _, mode := range []string{"empty", "commentary", "cutoff", "pause"} {
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
					if mode == "empty" {
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
				return phaseTestResponse(false, finalAnswerOutput)
			})
			a.Config = &Config{client: &client, MaxTurns: 10, ConfirmUnphased: mode == "empty", Tools: confirmationTestTools}
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
