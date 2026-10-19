package harness

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestCompleteRetainsTerminalMetadataAndOutput(t *testing.T) {
	message := func(id, text string) string {
		return fmt.Sprintf(`{"type":"message","id":%q,"role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}`, id, text)
	}
	done := itemDoneEvents(message("first", "first"), message("second", "second"))
	finalItems := []string{message("first", "first updated"), message("second", "second updated"), message("third", "third")}
	for _, tc := range []struct {
		name                  string
		output                []string
		completed, incomplete []string
	}{
		{"missing", nil, []string{"first", "second"}, []string{"first", "second"}},
		{"shorter", finalItems[:1], []string{"first", "second"}, []string{"first updated"}},
		{"same length", finalItems[:2], []string{"first updated", "second updated"}, []string{"first updated", "second updated"}},
		{"longer", finalItems, []string{"first updated", "second updated", "third"}, []string{"first updated", "second updated", "third"}},
	} {
		for _, terminal := range []struct {
			status string
			want   []string
		}{
			{"completed", tc.completed},
			{"incomplete", tc.incomplete},
		} {
			t.Run(terminal.status+"/"+tc.name, func(t *testing.T) {
				client := streamingTestClient(func(*http.Request) string {
					// The terminal event defines completion even when response.status
					// is absent. Some providers also omit or shorten the final output.
					return done + fmt.Sprintf(`data: {"type":"response.%s","response":{"id":"response-id","model":"response-model","stop_reason":"provider_reason","end_turn":false,"incomplete_details":{"reason":"max_output_tokens"},"output":[%s],"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":2}}}}`+"\n\n", terminal.status, strings.Join(tc.output, ","))
				})
				var visible []string
				resp, err := (&Config{client: &client}).complete(t.Context(), &request{}, func(m Message, _ error) bool {
					visible = append(visible, m.Content[0].Text)
					return true
				})
				if err != nil {
					t.Fatal(err)
				}
				var retained []string
				for _, m := range resp.messages {
					retained = append(retained, m.Content[0].Text)
				}
				if !slices.Equal(retained, terminal.want) || !slices.Equal(visible, terminal.want) {
					t.Fatalf("retained=%q visible=%q, want %q", retained, visible, terminal.want)
				}
				if resp.id != "response-id" || resp.model != "response-model" || resp.stopReason != "provider_reason" {
					t.Fatalf("terminal metadata lost: %+v", resp)
				}
				if want := (Usage{InputTokens: 11, OutputTokens: 7, CacheReadInputTokens: 5, ReasoningTokens: 2}); resp.usage != want {
					t.Fatalf("usage=%+v, want %+v", resp.usage, want)
				}
				if terminal.status == "incomplete" {
					if !resp.incomplete || resp.incompleteReason != "max_output_tokens" || resp.endTurn != nil || !slices.Equal(resp.finishReasons, []string{"length"}) {
						t.Fatalf("cutoff treated as completed: %+v", resp)
					}
				} else if resp.incomplete || resp.incompleteReason != "" || resp.endTurn == nil || *resp.endTurn || !slices.Equal(resp.finishReasons, []string{"stop"}) {
					t.Fatalf("completion boundary lost: %+v", resp)
				}
			})
		}
	}
}

func TestCompleteRetainsOnlyReplayableOutput(t *testing.T) {
	for _, doneEvents := range []bool{false, true} {
		for _, status := range []string{"completed", "incomplete", "in_progress", ""} {
			t.Run(fmt.Sprintf("done_events=%t/status=%s", doneEvents, status), func(t *testing.T) {
				statusField := ""
				if status != "" {
					statusField = fmt.Sprintf(`,"status":%q`, status)
				}
				client := streamingTestClient(func(*http.Request) string {
					stream := phaseTestResponse(doneEvents,
						`{"type":"reasoning","id":"r1","encrypted_content":"signature","summary":[]`+statusField+`}`,
						`{"type":"function_call","call_id":"c1","name":"probe","arguments":"{}"`+statusField+`}`,
						`{"type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"Partial answer"}]}`,
					)
					return strings.ReplaceAll(stream, "response.completed", "response.incomplete")
				})
				resp, err := (&Config{client: &client}).complete(t.Context(), &request{model: "test"}, func(Message, error) bool { return true })
				if err != nil {
					t.Fatal(err)
				}
				var reasoning, calls bool
				for _, m := range resp.messages {
					for _, c := range m.Content {
						reasoning = reasoning || c.Reasoning != nil
						calls = calls || c.ToolCall != nil
					}
				}
				if reasoning != (status == "completed" || status == "") || calls != (status == "completed") {
					t.Fatalf("unreplayable output retained: reasoning=%t calls=%t", reasoning, calls)
				}
				if !resp.incomplete || lastAssistantText(resp.messages) != "Partial answer" {
					t.Fatal("cutoff discarded partial assistant text")
				}
			})
		}
	}
}
