package agent

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

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
				resp, err := complete(t.Context(), &client, &request{model: "test"}, func(Message, error) bool { return true })
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
