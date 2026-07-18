package code

import (
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func TestCellContextStatsAlignment(t *testing.T) {
	stats := agent.ContextStats{
		Model:              "claude-fable-5",
		Window:             1_000_000,
		InstructionsTokens: 4200,
		ToolTokens:         17_600,
		ToolStats:          []agent.ToolStat{{Name: "shell", Tokens: 1400}, {Name: "agent", Tokens: 1100}},
		MessagesTokens:     18_700,
		MessageCount:       58,
		LastInputTokens:    42_500,
	}

	lines := cellContextStats(stats, 120)
	if len(lines) < contextGridRows {
		t.Fatalf("expected at least %d lines, got %d", contextGridRows, len(lines))
	}

	for i, line := range lines[1 : 1+contextGridRows] {
		if ansi.Width(line) < 39 {
			t.Fatalf("grid row %d too narrow: %q", i, line)
		}
	}

	narrow := cellContextStats(stats, 40)
	if len(narrow) < 8 {
		t.Fatalf("narrow rendering too short: %d lines", len(narrow))
	}
}
