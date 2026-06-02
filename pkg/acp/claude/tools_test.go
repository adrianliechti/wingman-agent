package claude

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestResultToTurn(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantStop acp.StopReason
		wantErr  bool
	}{
		{"success end_turn", `{"type":"result","subtype":"success","result":"done"}`, acp.StopReasonEndTurn, false},
		{"success max_tokens", `{"type":"result","subtype":"success","stop_reason":"max_tokens"}`, acp.StopReasonMaxTokens, false},
		{"success is_error", `{"type":"result","subtype":"success","is_error":true,"result":"boom"}`, "", true},
		{"login required", `{"type":"result","subtype":"success","result":"Please run /login first"}`, "", true},
		{"error_during_execution", `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["x","y"]}`, "", true},
		{"error_max_turns recoverable", `{"type":"result","subtype":"error_max_turns"}`, acp.StopReasonMaxTurnRequests, false},
		{"error_max_turns is_error", `{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["limit"]}`, "", true},
		{"unknown subtype falls back to end_turn", `{"type":"result","subtype":"weird"}`, acp.StopReasonEndTurn, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resultToTurn([]byte(tt.line))
			if tt.wantErr {
				if got.err == nil {
					t.Fatalf("expected error, got stop=%q", got.stop)
				}
				return
			}
			if got.err != nil {
				t.Fatalf("unexpected error: %v", got.err)
			}
			if got.stop != tt.wantStop {
				t.Errorf("stop = %q, want %q", got.stop, tt.wantStop)
			}
		})
	}
}

func TestResultLoginIsAuthRequired(t *testing.T) {
	got := resultToTurn([]byte(`{"type":"result","subtype":"success","result":"Please run /login"}`))
	re, ok := got.err.(*acp.RequestError)
	if !ok {
		t.Fatalf("want *acp.RequestError, got %T", got.err)
	}
	if re.Code != -32000 {
		t.Errorf("want auth-required code -32000, got %d", re.Code)
	}
}

func TestToolInfoEditProducesDiff(t *testing.T) {
	input := json.RawMessage(`{"file_path":"/proj/main.go","old_string":"foo","new_string":"bar"}`)
	info := toolInfoFromToolUse("Edit", input, "/proj")
	if info.kind != acp.ToolKindEdit {
		t.Errorf("kind = %q, want edit", info.kind)
	}
	if info.title != "Edit main.go" {
		t.Errorf("title = %q, want %q", info.title, "Edit main.go")
	}
	if len(info.content) != 1 || info.content[0].Diff == nil {
		t.Fatalf("expected one diff content block, got %+v", info.content)
	}
	d := info.content[0].Diff
	if d.Path != "/proj/main.go" || d.NewText != "bar" || d.OldText == nil || *d.OldText != "foo" {
		t.Errorf("diff = %+v", d)
	}
	if len(info.locations) != 1 || info.locations[0].Path != "/proj/main.go" {
		t.Errorf("locations = %+v", info.locations)
	}
}

func TestToolInfoReadTitleAndLocation(t *testing.T) {
	input := json.RawMessage(`{"file_path":"/proj/pkg/x.go","offset":10,"limit":5}`)
	info := toolInfoFromToolUse("Read", input, "/proj")
	if want := "Read pkg/x.go (10 - 14)"; info.title != want {
		t.Errorf("title = %q, want %q", info.title, want)
	}
	if len(info.locations) != 1 || info.locations[0].Line == nil || *info.locations[0].Line != 10 {
		t.Errorf("locations = %+v", info.locations)
	}
}

func TestToolInfoGrepLabel(t *testing.T) {
	input := json.RawMessage(`{"pattern":"todo","-i":true,"output_mode":"files_with_matches","glob":"*.go"}`)
	info := toolInfoFromToolUse("Grep", input, "")
	if want := `grep -i -l --include="*.go" "todo"`; info.title != want {
		t.Errorf("title = %q, want %q", info.title, want)
	}
	if info.kind != acp.ToolKindSearch {
		t.Errorf("kind = %q, want search", info.kind)
	}
}

func TestPlanEntriesFromTodoWrite(t *testing.T) {
	input := json.RawMessage(`{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"},{"content":"c","status":"pending"}]}`)
	entries, ok := planEntriesFromTodoWrite(input)
	if !ok || len(entries) != 3 {
		t.Fatalf("entries=%+v ok=%v", entries, ok)
	}
	if entries[0].Status != acp.PlanEntryStatusCompleted ||
		entries[1].Status != acp.PlanEntryStatusInProgress ||
		entries[2].Status != acp.PlanEntryStatusPending {
		t.Errorf("statuses = %+v", entries)
	}

	if _, ok := planEntriesFromTodoWrite(json.RawMessage(`{}`)); ok {
		t.Errorf("expected ok=false for missing todos")
	}
}

func TestMarkdownEscapeLengthensFence(t *testing.T) {
	// Text containing a 4-backtick fence must be wrapped in a 5-backtick fence.
	got := markdownEscape("````\ncode\n````")
	if want := "`````\n````\ncode\n````\n`````"; got != want {
		t.Errorf("markdownEscape = %q, want %q", got, want)
	}
}
