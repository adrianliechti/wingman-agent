package acp

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
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
		{name: "empty text with raw output", updates: [][]acpsdk.ToolCallUpdateOpt{{acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(""))}), acpsdk.WithUpdateRawOutput("raw output")}}, want: "raw output"},
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
			if got := result.Content[0].ToolResult.Content.Text(); got != tc.want {
				t.Fatalf("result = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolCompletionPreservesOrderedImages(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	content := []acpsdk.ToolCallContent{
		acpsdk.ToolContent(acpsdk.TextBlock("before")),
		acpsdk.ToolContent(acpsdk.ImageBlock(base64.StdEncoding.EncodeToString(pngBytes.Bytes()), "image/png")),
		acpsdk.ToolContent(acpsdk.TextBlock("after")),
	}
	for _, status := range []acpsdk.ToolCallStatus{acpsdk.ToolCallStatusCompleted, acpsdk.ToolCallStatusFailed} {
		for _, terminalStart := range []bool{false, true} {
			t.Run(string(status)+map[bool]string{false: "/update", true: "/start"}[terminalStart], func(t *testing.T) {
				a := &Agent{}
				s := &sessionState{toolCalls: map[string]toolCall{}}
				turn := &turn{ctx: t.Context(), events: make(chan event, 1)}
				const id acpsdk.ToolCallId = "image-call"
				update := acpsdk.StartToolCall(id, "read", acpsdk.WithStartContent(content), acpsdk.WithStartStatus(status))
				if !terminalStart {
					a.translateUpdate(s, turn, acpsdk.StartToolCall(id, "read"))
					a.translateUpdate(s, turn, acpsdk.UpdateToolCall(id, acpsdk.WithUpdateContent(content)))
					update = acpsdk.UpdateToolCall(id, acpsdk.WithUpdateStatus(status))
				}
				msg, ok := a.translateUpdate(s, turn, update)
				if !ok || len(msg.Content) != 1 || msg.Content[0].ToolResult == nil {
					t.Fatalf("missing result: %+v", msg)
				}
				result := msg.Content[0].ToolResult
				blocks := result.Content
				if len(blocks) != 3 || blocks[0].Text == nil || *blocks[0].Text != "before" || blocks[1].Image == nil || blocks[2].Text == nil || *blocks[2].Text != "after" {
					t.Fatalf("lost block order: %+v", blocks)
				}
				if !bytes.Equal(blocks[1].Image.Data, pngBytes.Bytes()) || blocks[1].Image.MIMEType != "image/png" || result.IsError != (status == acpsdk.ToolCallStatusFailed) {
					t.Fatalf("lost image or status: %+v", result)
				}
			})
		}
	}
}
