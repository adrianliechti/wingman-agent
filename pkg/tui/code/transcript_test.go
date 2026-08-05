package code

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestTranscriptInspectorSearchAndExpansion(t *testing.T) {
	messages := []agent.Message{
		{Role: agent.RoleUser, Content: []agent.Content{{Text: "find the needle"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{
			{ToolResult: &agent.ToolResult{ID: "tool-1", Name: "read", Args: `{"path":"file.go"}`, Content: "one\ntwo\nthree\nfour\nfive"}},
			{Reasoning: &agent.Reasoning{ID: "reason-1", Summary: "checking the needle carefully"}},
			{Text: "done"},
		}},
	}
	a := &App{ctx: context.Background(), agent: newUITestAgent(messages), sessionID: "session"}
	o := &transcriptOverlay{app: a, selected: -1, follow: true, expanded: map[string]bool{}, cache: map[string]transcriptCache{}}
	o.buildEntries()
	if len(o.entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(o.entries))
	}

	var toolIndex int
	for i, entry := range o.entries {
		if entry.kind == transcriptTool {
			toolIndex = i
			break
		}
	}
	o.selected = toolIndex
	collapsed := o.entryLines(o.entries[toolIndex], 80, true)
	o.toggleSelected()
	expanded := o.entryLines(o.entries[toolIndex], 80, true)
	if len(expanded) <= len(collapsed) {
		t.Fatalf("expanded tool has %d lines, collapsed has %d", len(expanded), len(collapsed))
	}

	o.query = "needle"
	o.updateMatches(true)
	if len(o.matches) != 2 {
		t.Fatalf("matches = %d, want user + reasoning", len(o.matches))
	}

	for _, width := range []int{40, 80, 120} {
		for i, line := range o.Render(width, 18) {
			if ansi.Width(line) > width {
				t.Fatalf("width %d row %d overflowed to %d", width, i, ansi.Width(line))
			}
		}
	}
}

func TestTranscriptUsesChatCellSpacing(t *testing.T) {
	longThought := strings.Repeat("weighing the tradeoffs carefully ", 6)
	messages := []agent.Message{
		{Role: agent.RoleAssistant, Content: []agent.Content{{ToolResult: &agent.ToolResult{
			ID: "tool-1", Name: "read", Content: "ok",
		}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Reasoning: &agent.Reasoning{
			ID: "reason-1", Summary: "considering the options",
		}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Reasoning: &agent.Reasoning{
			ID: "reason-2", Summary: longThought,
		}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Reasoning: &agent.Reasoning{
			ID: "reason-3", Summary: "settling on a plan",
		}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{ToolResult: &agent.ToolResult{
			ID: "tool-2", Name: "shell", Content: "one\ntwo\nthree\nfour",
		}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: "done"}}},
	}

	const transcriptWidth = 80
	a := &App{ctx: context.Background(), agent: newUITestAgent(messages), sessionID: "session"}
	var chat []string
	for _, message := range messages {
		chat = append(chat, a.formatMessageCells(message, transcriptWidth-2)...)
	}

	o := &transcriptOverlay{
		app: a, selected: -1, expanded: map[string]bool{}, cache: map[string]transcriptCache{},
	}
	o.buildEntries()
	for _, entry := range o.entries {
		if entry.kind == transcriptReasoning {
			o.expanded[entry.key] = true
		}
	}
	// Remove selection styling so only the transcript's fixed two-column
	// navigation gutter differs from the normal chat cells.
	o.selected = -1
	transcript, _, _ := o.bodyLines(transcriptWidth)
	for i, line := range transcript {
		if strings.HasPrefix(line, "  ") {
			transcript[i] = line[2:]
		}
	}

	if !slices.Equal(transcript, chat) {
		t.Fatalf("transcript spacing drifted from chat\ntranscript: %q\nchat:       %q", transcript, chat)
	}
}
