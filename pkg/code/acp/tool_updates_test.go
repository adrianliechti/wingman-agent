package acp

import (
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

func TestToolCompletionRetainsLatestOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   []acpsdk.ToolCallStartOpt
		updates [][]acpsdk.ToolCallUpdateOpt
		want    string
	}{
		{name: "start text", start: []acpsdk.ToolCallStartOpt{acpsdk.WithStartContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock("start output"))})}, want: "start output"},
		{name: "progress text", updates: [][]acpsdk.ToolCallUpdateOpt{{acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock("progress output"))})}}, want: "progress output"},
		{name: "progress raw output", updates: [][]acpsdk.ToolCallUpdateOpt{{acpsdk.WithUpdateRawOutput("raw output")}}, want: "raw output"},
		{name: "replacement text", updates: [][]acpsdk.ToolCallUpdateOpt{
			{acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock("old output"))})},
			{acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock("replacement output"))})},
		}, want: "replacement output"},
		{name: "explicit clear", start: []acpsdk.ToolCallStartOpt{acpsdk.WithStartContent([]acpsdk.ToolCallContent{acpsdk.ToolDiffContent("file.txt", "new", "old")})}, updates: [][]acpsdk.ToolCallUpdateOpt{{acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{})}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{}
			s := &sessionState{toolCalls: map[string]toolCall{}}
			turn := &turn{}
			const id acpsdk.ToolCallId = "call"
			a.translateUpdate(s, turn, acpsdk.StartToolCall(id, "tool", tc.start...))
			for _, opts := range tc.updates {
				a.translateUpdate(s, turn, acpsdk.UpdateToolCall(id, opts...))
			}
			result, ok := a.translateUpdate(s, turn, acpsdk.UpdateToolCall(id, acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted)))
			if !ok || len(result.Content) != 1 || result.Content[0].ToolResult == nil {
				t.Fatalf("missing tool result: %+v", result)
			}
			if got := result.Content[0].ToolResult.Content; got != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}
		})
	}
}
