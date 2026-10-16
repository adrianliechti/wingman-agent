package tool

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestPresentGlobUsesAbsolutePatternDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "src")
	pattern := filepath.Join(dir, "**", "*.go")
	args, err := json.Marshal(map[string]any{"path": "ignored", "pattern": pattern})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"glob", "Find files"} {
		got := Present(name, "", string(args), false)
		if got.Path != dir || got.Hint != pattern || got.Args != "" {
			t.Fatalf("%s presentation = %#v", name, got)
		}
	}
}

func TestPresentCompactsBuiltInTools(t *testing.T) {
	tests := []struct {
		name, kind, args string
		hasLocation      bool
		want             Presentation
	}{
		{
			name: "read", args: `{"file_path":"pkg/main.go","offset":12,"limit":4}`,
			want: Presentation{Title: "Read file", Kind: "read", Args: `{"limit":4}`, Path: "pkg/main.go", Line: 12},
		},
		{
			name: "exec", args: `{"command":"go test ./...","workdir":"pkg"}`,
			want: Presentation{Title: "Run command", Kind: "execute", Args: `{"workdir":"pkg"}`, Hint: "go test ./..."},
		},
		{
			name: "Find files", kind: "search", args: `{"pattern":"README*","limit":20}`,
			want: Presentation{Title: "Find files", Kind: "search", Args: `{"limit":20}`, Hint: "README*"},
		},
		{
			name: "spawn_agent", args: `{"prompt":"Review the tool UX","reasoning_effort":"high"}`,
			want: Presentation{Title: "Delegate task", Args: `{"reasoning_effort":"high"}`, Hint: "Review the tool UX"},
		},
		{
			name: "Follow up with agent", args: `{"message":"Continue the review","target":"reviewer"}`,
			want: Presentation{Title: "Follow up with agent", Args: `{"target":"reviewer"}`, Hint: "Continue the review"},
		},
		{
			name: "exec_session", args: `{"session_id":42,"input":"yes\n","wait":10}`,
			want: Presentation{Title: "Continue command", Kind: "execute", Args: `{"wait":10}`, Hint: "session 42: yes"},
		},
		{
			name: "agent_send", args: `{"id":"agent-1","message":"Check aliases"}`,
			want: Presentation{Title: "Follow up with agent", Hint: "agent-1: Check aliases"},
		},
		{
			name: "edit", args: `{"edits":[{"file_path":"pkg/a.go","old_string":"a","new_string":"b"},{"file_path":"pkg/b.go","old_string":"c","new_string":"d"}]}`,
			want: Presentation{Title: "Edit file", Kind: "edit", Path: "pkg/a.go", Hint: "2 edits · 2 files"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Present(tc.name, tc.kind, tc.args, tc.hasLocation); got != tc.want {
				t.Fatalf("Present() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestPresentDerivesHintsForHumanizedNames(t *testing.T) {
	input := Present("Request input", "", `{"questions":[{"question":"Which layout?"}]}`, false)
	if input.Hint != "Which layout?" || input.Args == "" {
		t.Fatalf("input presentation = %#v", input)
	}
	args := `{"session_id":42,"input":"yes\n","wait":10}`
	if got, want := Present("Continue command", "", args, false), Present("exec_session", "", args, false); got != want {
		t.Fatalf("humanized exec_session lost input: %#v, want %#v", got, want)
	}
}

func TestPresentSummarizesMultipleWebQueries(t *testing.T) {
	got := Present("Web search", "fetch", `{"queries":["first","second","third"],"allowed_domains":["example.com"]}`, false)
	if got.Hint != "first · second +1" || got.Args != `{"allowed_domains":["example.com"]}` {
		t.Fatalf("presentation = %#v", got)
	}
}

func TestPresentKeepsPartialInputUsable(t *testing.T) {
	got := Present("shell", "", `{"command":"go test ./`, false)
	if got.Title != "Run command" || got.Kind != "execute" || got.Hint != "go test ./" || got.Args == "" {
		t.Fatalf("presentation = %#v", got)
	}
}

func TestPresentRenamedToolsAndLegacyAliases(t *testing.T) {
	for _, tc := range []struct {
		name, legacy, args, title, hint string
	}{
		{"exec", "exec_command", `{"command":"go test ./..."}`, "Run command", "go test ./..."},
		{"search_web", "web_search", `{"query":"Go documentation"}`, "Web search", "Go documentation"},
		{"elicit", "ask", `{"questions":[{"question":"Which layout?"}]}`, "Request input", "Which layout?"},
		{"agent_output", "task_output", `{"id":"agent-1"}`, "Read agent output", "agent-1"},
		{"agent_send", "task_send", `{"id":"agent-1","message":"Check aliases"}`, "Follow up with agent", "agent-1: Check aliases"},
		{"agent_stop", "task_stop", `{"id":"agent-1"}`, "Stop agent", "agent-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Present(tc.name, "", tc.args, false)
			if got.Title != tc.title || got.Hint != tc.hint {
				t.Fatalf("presentation = %#v", got)
			}
			if legacy := Present(tc.legacy, "", tc.args, false); legacy != got {
				t.Fatalf("legacy presentation = %#v, want %#v", legacy, got)
			}
		})
	}
}
