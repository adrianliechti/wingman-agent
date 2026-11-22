package harness

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func pausedResponse(items ...string) string {
	return strings.Replace(phaseTestResponse(false, items...), `"response":{`, `"response":{"stop_reason":"pause_turn",`, 1)
}

func TestNativePauseContinuesIntoTools(t *testing.T) {
	for _, confirmUnphased := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "automatic confirmation"}[confirmUnphased], func(t *testing.T) {
			requests, runs, stops := 0, 0, 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				switch requests {
				case 1:
					return pausedResponse(strings.ReplaceAll(commentaryOutput, `,"phase":"commentary"`, ""))
				case 2:
					return phaseTestResponse(false, `{"type":"function_call","id":"fc_check","call_id":"check","name":"check","arguments":"{}","status":"completed"}`)
				case 3:
					return phaseTestResponse(false, finalAnswerOutput)
				default:
					t.Fatal("unexpected extra request")
					return ""
				}
			})
			a := &Agent{Config: &Config{client: &client, ConfirmUnphased: confirmUnphased, Tools: func() []tool.Tool {
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
	paused := pausedResponse(commentaryOutput)
	for _, tc := range []struct{ name, reply string }{
		{"stop reason alone", paused},
		{"stop reason and explicit continuation", strings.Replace(paused, `"response":{`, `"response":{"end_turn":false,`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				return tc.reply
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 6}}
			err := runLoopTurn(t.Context(), a, "Check it")
			if !errors.Is(err, errRepeatedPause) || requests != maxAutomaticContinuations+1 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
			assertIncompleteTurn(t, a)
		})
	}
}
