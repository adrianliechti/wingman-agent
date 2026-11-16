package server

import (
	"bytes"
	"image"
	"image/png"
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestTranscriptPreservesLegacyHistoryIdentities(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	attachment := &tool.ImageContent{MIMEType: "image/png", Data: data.Bytes()}
	entries := transcriptEntries([]harness.Message{
		{Role: harness.RoleUser, Hidden: true, Content: []harness.Content{{Text: "model context"}}},
		{Role: harness.RoleUser, InputID: "question", Content: []harness.Content{{Text: "hidden", Hidden: true}, {Text: "question"}}},
		{Role: harness.RoleAssistant, Content: []harness.Content{
			{Text: "hidden", Hidden: true},
			{},
			{ToolCall: &harness.ToolCall{ID: "call", Name: "read"}},
			{ToolResult: &harness.ToolResult{ID: "call", Name: "read", Content: tool.Content{tool.TextBlock("result"), {Image: attachment}}}},
			{Reasoning: &harness.Reasoning{Summary: "reason"}},
			{Refusal: "answer"},
		}},
	})
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	// These persisted identities include the extra content slot used by a tool
	// image, and exclude hidden messages and parts. Reconnecting must keep them.
	want := []string{"input:question", "tool:call", "history:1:3:image", "history:1:4:reasoning", "history:1:5:text"}
	if !slices.Equal(ids, want) {
		t.Fatalf("entry identities = %q, want %q", ids, want)
	}
	if entries[0].Content != "question" || entries[1].ToolResult == nil || *entries[1].ToolResult != "result" ||
		!slices.Equal(entries[2].Images, []string{attachment.DataURL()}) || entries[3].Content != "reason" || entries[4].Content != "answer" {
		t.Fatalf("history projection = %+v", entries)
	}
}
