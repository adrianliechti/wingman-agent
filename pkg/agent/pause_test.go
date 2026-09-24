package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func pausedResponse(items ...string) string {
	return strings.Replace(phaseTestResponse(false, items...), `"response":{`, `"response":{"stop_reason":"pause_turn",`, 1)
}

func TestNativePauseContinuesIntoTools(t *testing.T) {
	for _, requireFinish := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "explicit finish"}[requireFinish], func(t *testing.T) {
			requests, runs, stops := 0, 0, 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				switch requests {
				case 1:
					return pausedResponse(strings.ReplaceAll(commentaryOutput, `,"phase":"commentary"`, ""))
				case 2:
					return phaseTestResponse(false, `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`)
				case 3:
					if requireFinish {
						return phaseTestResponse(false, finalAnswerOutput, finishOutput("done", "{}"))
					}
					return phaseTestResponse(false, finalAnswerOutput)
				default:
					t.Fatal("unexpected extra request")
					return ""
				}
			})
			a := &Agent{Config: &Config{client: &client, RequireFinish: func(string) bool { return requireFinish }, Tools: func() []tool.Tool {
				return []tool.Tool{{Name: "check", Execute: func(context.Context, map[string]any) (tool.Result, error) { runs++; return tool.Text("PASS"), nil }}}
			}, Hooks: hook.Hooks{Stop: []hook.Stop{func(context.Context, string, bool) (hook.Outcome, error) { stops++; return hook.Outcome{}, nil }}}}}
			stream, err := a.Send(t.Context(), []Content{{Text: "Check it"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			if requests != 3 || runs != 1 || stops != 1 {
				t.Fatalf("requests=%d runs=%d stops=%d", requests, runs, stops)
			}
		})
	}
}

func TestNativePauseRecoveryIsBounded(t *testing.T) {
	requests := 0
	client := streamingTestClient(func(*http.Request) string { requests++; return pausedResponse(commentaryOutput) })
	a := &Agent{Config: &Config{client: &client}}
	stream, err := a.Send(t.Context(), []Content{{Text: "Check it"}})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		runErr = errors.Join(runErr, err)
	}
	if !errors.Is(runErr, ErrTurnIncomplete) || requests != 3 {
		t.Fatalf("error=%v requests=%d", runErr, requests)
	}
	assertIncompleteTurn(t, a)
}
