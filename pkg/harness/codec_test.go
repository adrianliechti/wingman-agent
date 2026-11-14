package harness

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func TestReasoningToInputReplaysEncryptedContent(t *testing.T) {
	p := reasoningToInput(&Reasoning{ID: "rs_1", Summary: "sum", Content: "blob", Model: "gpt-5.5"})

	if p == nil {
		t.Fatal("expected reasoning item to replay")
	}
	if p.ID != "rs_1" {
		t.Fatalf("ID = %q, want rs_1", p.ID)
	}
	if !p.EncryptedContent.Valid() || p.EncryptedContent.Value != "blob" {
		t.Fatalf("EncryptedContent = %#v, want blob", p.EncryptedContent)
	}
	if len(p.Summary) != 1 || p.Summary[0].Text != "sum" {
		t.Fatalf("Summary = %#v, want single part", p.Summary)
	}
}

func TestReasoningToInputSkipsUnreplayableItems(t *testing.T) {
	cases := []struct {
		name string
		r    *Reasoning
	}{
		{"nil", nil},
		{"no encrypted content", &Reasoning{ID: "rs_1", Summary: "sum"}},
		{"no id", &Reasoning{Summary: "sum", Content: "blob"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := reasoningToInput(tc.r); p != nil {
				t.Fatalf("expected nil, got %#v", p)
			}
		})
	}
}

func TestFromReasoningCapturesEncryptedContentAndSummaryParts(t *testing.T) {
	msg, ok := fromReasoning(&responses.ResponseReasoningItemParam{
		ID:               "rs_1",
		EncryptedContent: openai.String("blob"),
		Summary: []responses.ResponseReasoningItemSummaryParam{
			{Text: "part one"},
			{Text: "part two"},
		},
	})

	if !ok {
		t.Fatal("expected reasoning item to convert")
	}
	r := msg.Content[0].Reasoning
	if r.Content != "blob" {
		t.Fatalf("Content = %q, want blob", r.Content)
	}
	if r.Summary != "part one\n\npart two" {
		t.Fatalf("Summary = %q", r.Summary)
	}
}

func TestFromReasoningKeepsEncryptedOnlyItems(t *testing.T) {
	msg, ok := fromReasoning(&responses.ResponseReasoningItemParam{
		ID:               "rs_1",
		EncryptedContent: openai.String("blob"),
	})

	if !ok || msg.Content[0].Reasoning.Content != "blob" {
		t.Fatal("expected summary-less encrypted item to be kept")
	}
}

func TestFromReasoningDropsEmptyItems(t *testing.T) {
	if _, ok := fromReasoning(&responses.ResponseReasoningItemParam{ID: "rs_1"}); ok {
		t.Fatal("expected item without summary or content to be dropped")
	}
}

func TestOutputMessageTextPreservesStableID(t *testing.T) {
	msg, ok := fromOutput(&responses.ResponseOutputMessageParam{
		ID: "message-1",
		Content: []responses.ResponseOutputMessageContentUnionParam{{
			OfOutputText: &responses.ResponseOutputTextParam{Text: "answer"},
		}},
	})
	if !ok || msg.Content[0].TextID != "message-1" {
		t.Fatalf("converted message = %+v", msg)
	}

	items, _ := assistantToInput(msg)
	if len(items) != 1 || items[0].OfOutputMessage == nil {
		t.Fatalf("replayed items = %+v", items)
	}
	if items[0].OfOutputMessage.ID != "" {
		t.Fatalf("provider-local text ID was replayed: %+v", items)
	}
}

func TestEasyInputPreservesOnlyAssistantPhase(t *testing.T) {
	for _, role := range []responses.EasyInputMessageRole{
		responses.EasyInputMessageRoleAssistant,
		responses.EasyInputMessageRoleUser,
	} {
		t.Run(string(role), func(t *testing.T) {
			item := responses.ResponseInputItemParamOfMessage("text", role)
			item.OfMessage.Phase = responses.EasyInputMessagePhaseCommentary
			messages := toMessages([]responses.ResponseInputItemUnionParam{item})
			if len(messages) != 1 {
				t.Fatalf("messages = %+v", messages)
			}
			want := MessagePhase("")
			if role == responses.EasyInputMessageRoleAssistant {
				want = PhaseCommentary
			}
			if messages[0].Phase != want {
				t.Fatalf("phase = %q, want %q", messages[0].Phase, want)
			}
		})
	}
}

func TestToInputFlushesImagesAfterToolResultRun(t *testing.T) {
	messages := []Message{
		{Role: RoleAssistant, Content: []Content{
			{ToolCall: &ToolCall{ID: "c1", Name: "view_image"}},
			{ToolCall: &ToolCall{ID: "c2", Name: "read"}},
		}},
		{Role: RoleAssistant, Content: []Content{
			{ToolResult: &ToolResult{ID: "c1", Name: "view_image", Content: tool.TextContent("[image attached below]")}},
			{File: &File{Data: "data:image/png;base64,AAAA"}},
		}},
		{Role: RoleAssistant, Content: []Content{
			{ToolResult: &ToolResult{ID: "c2", Name: "read", Content: tool.TextContent("file contents")}},
		}},
	}

	items := toInput(messages)

	var kinds []string
	for _, item := range items {
		switch {
		case item.OfFunctionCall != nil:
			kinds = append(kinds, "call")
		case item.OfFunctionCallOutput != nil:
			kinds = append(kinds, "output")
		case item.OfInputMessage != nil:
			kinds = append(kinds, "image")
		default:
			kinds = append(kinds, "other")
		}
	}

	want := []string{"call", "call", "output", "output", "image"}
	if len(kinds) != len(want) {
		t.Fatalf("items = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("items = %v, want %v", kinds, want)
		}
	}
}

// A tool result is one wire item. Its images travel inside the output, in
// order, so they stay attached to the call that produced them; plain text
// keeps the string form every backend accepts.
func TestToolOutputCarriesAttachmentsInOrder(t *testing.T) {
	image := imageContentFixture(t)
	url := image[0].Image.DataURL()
	mixed := tool.Content{tool.TextBlock("before"), image[0], tool.TextBlock(""), tool.TextBlock("after")}

	messages := []Message{
		{Role: RoleAssistant, Content: []Content{
			{ToolCall: &ToolCall{ID: "shot", Name: "browser_screenshot", Args: "{}"}},
			{ToolCall: &ToolCall{ID: "text", Name: "read", Args: "{}"}},
		}},
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "shot", Name: "browser_screenshot", Content: mixed}}}},
		{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{ID: "text", Name: "read", Content: tool.Content{tool.TextBlock("line one"), tool.TextBlock("line two")}}}}},
	}
	items := toInput(messages)
	if len(items) != 4 {
		t.Fatalf("attachments must not add wire items: got %d items", len(items))
	}

	raw, err := json.Marshal(items[2])
	if err != nil {
		t.Fatal(err)
	}
	var shot struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
			Detail   string `json:"detail"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &shot); err != nil {
		t.Fatalf("output is not a part list: %v\n%s", err, raw)
	}
	if shot.Type != "function_call_output" || shot.CallID != "shot" || len(shot.Output) != 3 {
		t.Fatalf("unexpected item: %s", raw)
	}
	if shot.Output[0].Type != "input_text" || shot.Output[0].Text != "before" ||
		shot.Output[1].Type != "input_image" || shot.Output[1].ImageURL != url || shot.Output[1].Detail != "auto" ||
		shot.Output[2].Type != "input_text" || shot.Output[2].Text != "after" {
		t.Fatalf("parts lost or reordered: %s", raw)
	}

	raw, err = json.Marshal(items[3])
	if err != nil {
		t.Fatal(err)
	}
	var plain struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &plain); err != nil || plain.Output != "line one\nline two" {
		t.Fatalf("text-only output must stay a string: %v\n%s", err, raw)
	}

	// The reverse mapping restores the same blocks.
	restored := toMessages(items)
	got := restored[len(restored)-2].Content[0].ToolResult
	want := tool.Content{tool.TextBlock("before"), image[0], tool.TextBlock("after")}
	if got == nil || got.Name != "browser_screenshot" || !reflect.DeepEqual(got.Content, want) {
		t.Fatalf("round trip changed the result: %+v", got)
	}
}
