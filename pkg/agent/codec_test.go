package agent

import (
	"testing"

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
