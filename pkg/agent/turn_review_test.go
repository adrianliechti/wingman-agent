package agent

import (
	"encoding/json"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"testing"
)

func reviewResult(sequence uint64, name, args string, metadata map[string]any) RuntimeEvent {
	return RuntimeEvent{Type: EventMessage, Sequence: sequence, Message: &Message{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{Name: name, Args: args, Metadata: metadata}}}}}
}
func TestTurnReviewUsesDurableEditsAndActualValidationExits(t *testing.T) {
	edit := reviewResult(3, "edit", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "main.go", Before: "user work", After: "agent work", BeforeExists: true, AfterExists: true, Mode: 0644}}})
	check := reviewResult(4, "exec_command", `{"command":"go test ./...","validation":true}`, map[string]any{"exit_code": 0})
	events := []RuntimeEvent{{Type: EventTurnStarted, TurnID: "turn"}, {Type: EventMessage, Message: &Message{Role: RoleUser, InputID: "input", Content: []Content{{Text: "work"}}}}, edit, check, {Type: EventContextCheckpoint}, {Type: EventTurnTerminal, TurnID: "turn", Terminal: &RuntimeTerminal{Status: RuntimeCompleted}}}
	for _, disk := range []bool{false, true} {
		source := events
		if disk {
			data, _ := json.Marshal(events)
			if err := json.Unmarshal(data, &source); err != nil {
				t.Fatal(err)
			}
		}
		reviews := BuildTurnReviews(source)
		if len(reviews) != 1 || reviews[0].InputID != "input" || reviews[0].Files[0].Before != "user work" || reviews[0].Validation != "passed" {
			t.Fatalf("review = %+v", reviews)
		}
	}
	events[2], events[3] = check, edit
	events[2].Sequence = 3
	events[3].Sequence = 4
	if review := BuildTurnReviews(events)[0]; review.Validation != "outdated" {
		t.Fatalf("stale checks passed: %+v", review)
	}
}
func TestTurnReviewDoesNotTrustSuccessTextOrForeignMetadata(t *testing.T) {
	for _, metadata := range []map[string]any{nil, {"exit_code": nil}, {"exit_code": "0"}, {"exit_code": 0.5}, {"exit_code": 1}} {
		events := []RuntimeEvent{{Type: EventTurnStarted, TurnID: "turn"}, reviewResult(2, "exec_command", `{"command":"go test","validation":true}`, metadata)}
		events[1].Message.Content[0].ToolResult.Content = "all tests passed"
		if review := BuildTurnReviews(events)[0]; review.Validation == "passed" {
			t.Fatalf("trusted text: %+v", review)
		}
	}
	events := []RuntimeEvent{{Type: EventTurnStarted, TurnID: "turn"}, reviewResult(2, "mcp__untrusted", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "file", Before: "malicious"}}})}
	if review := BuildTurnReviews(events)[0]; len(review.Files) != 0 {
		t.Fatal("trusted foreign edit attribution")
	}
}

func TestTurnReviewMarksEachOutdatedCheckBeforePassingItToParent(t *testing.T) {
	events := []RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "child"},
		reviewResult(2, "exec_command", `{"command":"go vet","validation":true}`, map[string]any{"exit_code": 0}),
		reviewResult(3, "edit", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "file", Before: "old", After: "new"}}}),
		reviewResult(4, "exec_command", `{"command":"go test","validation":true}`, map[string]any{"exit_code": 1}),
	}
	child := BuildTurnReviews(events)[0]
	if child.Checks[0].Outcome != "outdated" {
		t.Fatalf("stale check still shown as passing: %+v", child)
	}
	parent := BuildTurnReviews([]RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "parent"},
		reviewResult(2, "agent", "", map[string]any{ChildReviewsMetadata: []TurnReview{child}}),
		reviewResult(3, "exec_command", `{"command":"go test","validation":true}`, map[string]any{"exit_code": 0}),
	})[0]
	if parent.Validation != "outdated" {
		t.Fatalf("rerunning one check hid another stale check: %+v", parent)
	}
}

func TestTurnReviewChildCheckSupersedesEarlierAttempt(t *testing.T) {
	child := TurnReview{Checks: []ValidationCheck{{Command: "go test", Outcome: "passed"}}, Validation: "passed"}
	review := BuildTurnReviews([]RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "parent"},
		reviewResult(2, "exec_command", `{"command":"go test","validation":true}`, map[string]any{"exit_code": 1}),
		reviewResult(3, "agent", "", map[string]any{ChildReviewsMetadata: []TurnReview{child}}),
	})[0]
	if review.Validation != "passed" || len(review.Checks) != 1 {
		t.Fatalf("successful retry did not replace the failed attempt: %+v", review)
	}
}

func TestInlineChildReviewPreservesEditsAndValidationOrder(t *testing.T) {
	child := TurnReview{Files: []tool.FileChange{{Path: "file", Before: "user", After: "agent"}}, Checks: []ValidationCheck{{Command: "go test", Outcome: "passed"}}, Validation: "outdated"}
	events := []RuntimeEvent{{Type: EventTurnStarted, TurnID: "parent"}, reviewResult(2, "agent", "", map[string]any{ChildReviewsMetadata: []TurnReview{child}})}
	got := BuildTurnReviews(events)[0]
	if len(got.Files) != 1 || got.Files[0].Before != "user" || got.Validation != "outdated" {
		t.Fatalf("child review lost: %+v", got)
	}
	events[1].Message.Content[0].ToolResult.Name = "mcp__foreign"
	got = BuildTurnReviews(events)[0]
	if len(got.Files) != 0 || len(got.Checks) != 0 {
		t.Fatal("foreign tool forged a child review")
	}
}

func TestTurnReviewOrdersChangesWithinOneLedgerEvent(t *testing.T) {
	edit := reviewResult(2, "edit", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "file", Before: "old", After: "new"}}})
	check := reviewResult(2, "exec_command", `{"command":"go test","validation":true}`, map[string]any{"exit_code": 0})
	for _, child := range []bool{false, true} {
		event := check
		event.Message = &Message{Content: append(append([]Content{}, check.Message.Content...), edit.Message.Content...)}
		if child {
			checked := BuildTurnReviews([]RuntimeEvent{{Type: EventTurnStarted, TurnID: "checked"}, check})[0]
			edited := BuildTurnReviews([]RuntimeEvent{{Type: EventTurnStarted, TurnID: "edited"}, edit})[0]
			event = reviewResult(2, "agent", "", map[string]any{ChildReviewsMetadata: []TurnReview{checked, edited}})
		}
		got := BuildTurnReviews([]RuntimeEvent{{Type: EventTurnStarted, TurnID: "parent"}, event})[0]
		if got.Validation != "outdated" || got.Checks[0].Outcome != "outdated" {
			t.Fatalf("child=%v: checks preceding edits were shown as passing: %+v", child, got)
		}
	}
}

func TestTurnReviewKeepsChecksInDifferentDirectoriesSeparate(t *testing.T) {
	failed := reviewResult(2, "exec_command", `{"command":"npm test","workdir":"frontend","validation":true}`, map[string]any{"exit_code": 1})
	passed := reviewResult(3, "exec_command", `{"command":"npm test","workdir":"backend","validation":true}`, map[string]any{"exit_code": 0})
	for _, child := range []bool{false, true} {
		event := passed
		if child {
			checks := BuildTurnReviews([]RuntimeEvent{{Type: EventTurnStarted, TurnID: "child"}, passed})
			event = reviewResult(3, "agent", "", map[string]any{ChildReviewsMetadata: checks})
		}
		review := BuildTurnReviews([]RuntimeEvent{{Type: EventTurnStarted, TurnID: "parent"}, failed, event})[0]
		if review.Validation != "failed" || len(review.Checks) != 2 {
			t.Fatalf("child=%v: another directory hid a failing check: %+v", child, review)
		}
	}
}

func TestTurnReviewPollingCannotRefreshChecksStartedBeforeEdits(t *testing.T) {
	metadata := map[string]any{"session_id": 1, "command": "npm test", "workdir": "/workspace/frontend", "validation": true}
	started := reviewResult(2, "exec_command", `{"command":"npm test","workdir":"frontend","validation":true}`, metadata)
	pending := reviewResult(4, "exec_session", `{"session_id":1}`, metadata)
	completed := reviewResult(5, "exec_session", `{"session_id":1}`, map[string]any{
		"session_id": 1, "command": "npm test", "workdir": "/workspace/frontend", "validation": true, "exit_code": 0,
	})
	edit := reviewResult(3, "edit", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "file", Before: "old", After: "new"}}})
	for _, changed := range []bool{false, true} {
		events := []RuntimeEvent{{Type: EventTurnStarted, TurnID: "turn"}, started}
		if changed {
			events = append(events, edit)
		}
		events = append(events, pending, completed)
		review := BuildTurnReviews(events)[0]
		want := "passed"
		if changed {
			want = "outdated"
		}
		if review.Validation != want || len(review.Checks) != 1 || review.Checks[0].WorkDir != "/workspace/frontend" {
			t.Fatalf("changed=%v: polled validation lost its launch context: %+v", changed, review)
		}
	}
}

func TestTurnReviewLateExitCannotReplaceANewerCheck(t *testing.T) {
	review := BuildTurnReviews([]RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "turn"},
		reviewResult(2, "exec_command", `{"command":"go test","validation":true}`, map[string]any{"session_id": 1}),
		reviewResult(3, "exec_command", `{"command":"go test","validation":true}`, map[string]any{"session_id": 2}),
		reviewResult(4, "exec_session", `{"session_id":1}`, map[string]any{"session_id": 1, "command": "go test", "validation": true, "exit_code": 0}),
	})[0]
	if review.Validation != "not_confirmed" || len(review.Checks) != 1 {
		t.Fatalf("an older process exit hid the still-running check: %+v", review)
	}
}

func TestTurnReviewChildCannotConfirmAnUnknownCommandLaunch(t *testing.T) {
	child := BuildTurnReviews([]RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "child"},
		reviewResult(2, "exec_session", `{"session_id":1}`, map[string]any{"session_id": 1, "command": "go test", "validation": true, "exit_code": 0}),
	})
	review := BuildTurnReviews([]RuntimeEvent{
		{Type: EventTurnStarted, TurnID: "parent"},
		reviewResult(2, "edit", "", map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "file", Before: "old", After: "new"}}}),
		reviewResult(3, "agent", "", map[string]any{ChildReviewsMetadata: child}),
	})[0]
	if review.Validation != "not_confirmed" {
		t.Fatalf("unknown launch was treated as checking the latest edits: %+v", review)
	}
}
