package agent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// Ported from pi's durable compaction tests: a user message that steers the
// agent while a tool batch is still open is not a valid place to start the
// verbatim window, because the result that follows would lose its call.
func TestRetainedWindowSkipsUserSteerInsideToolBatch(t *testing.T) {
	call := ToolCall{ID: "c1", Name: "read", Args: `{"file_path":"a.go"}`}
	messages := []Message{
		{Role: RoleUser, Content: []Content{{Text: "task"}}},          // 0
		toolCallMessage(call),                                         // 1
		{Role: RoleUser, Content: []Content{{Text: "steer: also b"}}}, // 2
		toolResultMessage(call, tool.Text("observed")),                // 3
		{Role: RoleAssistant, Content: []Content{{Text: "done"}}},     // 4
	}
	cost := func(from, to int) int { return messagesTokens(messages[from:to]) }
	for _, tc := range []struct {
		name   string
		budget int
		want   int
	}{
		{"steer and result fit but stay with the call", cost(2, 5), 4},
		{"the call itself is not a boundary", cost(1, 5), 4},
		{"the whole batch fits from the original task", cost(0, 5), 0},
	} {
		if got := retainedWindowStart(messages, tc.budget); got != tc.want {
			t.Errorf("%s: start = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Ported from codex's collect_user_messages_filters_session_prefix_entries:
// host guidance and harness notices are user-role messages on the wire but
// never count as the user's own input when rebuilding compacted history.
func TestRetainedUsersSkipHostContextAndNotices(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: []Content{{Text: "task"}}},
		hiddenContextMessage(sessionContextPrefix + "project guidance"),
		hiddenContextMessage("harness notice"),
		{Role: RoleAssistant, Content: []Content{{Text: "working"}}},
		{Role: RoleUser, Content: []Content{{Text: "next"}}},
	}
	retained := retainedUserMessages(messages, messagesTokens(messages))
	if len(retained) != 2 || contentText(retained[0].Content) != "task" || contentText(retained[1].Content) != "next" {
		t.Fatalf("retained = %+v", retained)
	}
}

// Ported from codex's insert_initial_context_before_last_real_user_or_summary
// tests: without a recent window the checkpoint stays last and the current
// guidance precedes the latest user input; with a window the guidance sits
// between the retained input and the checkpoint.
func TestCompactionPlacesGuidanceAndCheckpoint(t *testing.T) {
	for _, window := range []bool{false, true} {
		name := "without_window"
		if window {
			name = "with_window"
		}
		t.Run(name, func(t *testing.T) {
			client := streamingTestClient(func(r *http.Request) string {
				var req struct {
					Input json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(req.Input), summaryRequest[:40]) {
					t.Fatal("expected only a summary request")
				}
				return phaseTestResponse(false, strings.ReplaceAll(finalAnswerOutput, "Checked and fixed.", "SUMMARY"))
			})
			recent := strings.Repeat("new ", 4000)
			if window {
				recent = "recent progress"
			}
			a := &Agent{Config: &Config{client: &client, ContextWindow: 10_000, ReserveTokens: 1000}, Messages: []Message{
				{Role: RoleUser, Content: []Content{{Text: "older task"}}},
				{Role: RoleAssistant, Content: []Content{{Text: strings.Repeat("old ", 20_000)}}},
				hiddenContextMessage(sessionContextPrefix + "current guidance"),
				{Role: RoleUser, Content: []Content{{Text: "latest task"}}},
				{Role: RoleAssistant, Content: []Content{{Text: recent}}},
			}}
			req := &request{messages: a.requestMessages()}
			a.anchorContextUsage(req, &response{usage: Usage{InputTokens: 10_000}})
			if err := a.prepareRequest(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range req.messages {
				switch {
				case isCompactionSummary(m):
					got = append(got, "checkpoint")
				case isSessionContext(m):
					got = append(got, "guidance")
				default:
					got = append(got, contentText(m.Content))
				}
			}
			want := "older task,guidance,latest task,checkpoint"
			if window {
				want = "older task,guidance,checkpoint,latest task,recent progress"
			}
			if strings.Join(got, ",") != want {
				t.Fatalf("layout = %q, want %q", strings.Join(got, ","), want)
			}
		})
	}
}

// Ported from pi's compaction-serialization tests: the briefing transcript
// bounds tool evidence per result but keeps the user's own words whole.
func TestBriefingTranscriptBoundsToolEvidenceNotUserInput(t *testing.T) {
	call := ToolCall{ID: "c1", Name: "read", Args: `{"file_path":"a.go"}`}
	long := strings.Repeat("y", 5000)
	short := strings.Repeat("s", 1500)
	messages := []Message{
		{Role: RoleUser, Content: []Content{{Text: "USER_START " + strings.Repeat("u", 5000) + " USER_END"}}},
		toolCallMessage(call),
		toolResultMessage(call, tool.Text(long)),
		toolResultMessage(call, tool.Text(short)),
		{Role: RoleAssistant, Content: []Content{{Text: "ASSISTANT_START " + strings.Repeat("a", 5000) + " ASSISTANT_END"}}},
	}
	transcript := recoveryTranscript(messages, maxSummarizeBytes)
	if !strings.Contains(transcript, "[user]: USER_START "+strings.Repeat("u", 5000)+" USER_END") {
		t.Fatal("user input was cut")
	}
	if !strings.Contains(transcript, "[tool call]: read("+call.Args+")") {
		t.Fatal("tool call missing or reformatted")
	}
	if !strings.Contains(transcript, "[tool result; error=false]: "+short) {
		t.Fatal("short tool result was cut")
	}
	if strings.Contains(transcript, long) || !strings.Contains(transcript, "[... content omitted ...]") {
		t.Fatal("long tool result was not bounded")
	}
	if strings.Count(transcript, "y") > 2000 {
		t.Fatalf("long tool result kept %d bytes", strings.Count(transcript, "y"))
	}
	if !strings.Contains(transcript, "ASSISTANT_START") || !strings.Contains(transcript, "ASSISTANT_END") || strings.Contains(transcript, strings.Repeat("a", 5000)) {
		t.Fatal("assistant text must keep both ends within its allowance")
	}
}
