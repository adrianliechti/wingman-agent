package server

import (
	"slices"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func warnTestEntries(c *sessionController) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var entries []string
	for _, e := range c.entries {
		entries = append(entries, e.Type+":"+e.Content)
	}
	return entries
}

// An agent warning is a transcript notice after the input that raised it, and
// it keeps that place when the committed history replaces the live entries.
func TestWarnAddsNoticeThatSurvivesHistoryReplacement(t *testing.T) {
	_, b, a, c, ch := reviewSessionsAttach(t)
	history := []harness.Message{
		{Role: harness.RoleUser, InputID: "t0", Content: []harness.Content{{Text: "earlier"}}},
		{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "done", TextID: "a0"}}},
	}
	a.mu.Lock()
	a.messages = history
	a.mu.Unlock()
	c.replaceHistory()

	reviewSessionsInput(c, "t1", "hello", "user", code.TurnInputActive)
	b.Warn("", "a warning without a session is dropped")
	b.Warn("saved", "Loaded only 10 of 20 bytes from AGENTS.md")
	c.apply(Frame{Type: EvtTextDelta, ID: "a1", Text: "answer"})
	want := []string{"user:earlier", "assistant:done", "user:hello", "notice:Warning: Loaded only 10 of 20 bytes from AGENTS.md", "assistant:answer"}
	if got := warnTestEntries(c); !slices.Equal(got, want) {
		t.Fatalf("live entries = %q", got)
	}
	if d := reviewSessionsDiverged(c, ch); d != "" {
		t.Fatalf("AHP projection diverged: %s", d)
	}

	a.mu.Lock()
	a.messages = append(history,
		harness.Message{Role: harness.RoleUser, InputID: "t1", Content: []harness.Content{{Text: "hello"}}},
		harness.Message{Role: harness.RoleAssistant, Content: []harness.Content{{Text: "answer", TextID: "a1"}}},
	)
	a.mu.Unlock()
	c.replaceHistory()
	if got := warnTestEntries(c); !slices.Equal(got, want) {
		t.Fatalf("entries after history replacement = %q", got)
	}
}

// A warning raised while a compaction runs joins that turn, so the compaction
// result is still recorded with it.
func TestWarnDuringCompactionKeepsCompactionNotice(t *testing.T) {
	_, b, _, c, _ := reviewSessionsAttach(t)
	input := turnQueueEntry(code.TurnInput{ID: "c1", Compact: true, Content: []harness.Content{{Text: "/compact"}}}, code.TurnInputActive, 0)
	c.apply(Frame{Type: EvtTurnInput, Input: &input})
	b.Warn("saved", "Skipped AGENTS.md (30000 bytes)")
	c.mu.Lock()
	c.compactedLocked("c1", harness.CompactResult{TokensBefore: 900, TokensAfter: 90})
	c.mu.Unlock()

	want := []string{"user:/compact", "notice:Warning: Skipped AGENTS.md (30000 bytes)", "notice:Context compacted · ~900 → ~90 tokens"}
	if got := warnTestEntries(c); !slices.Equal(got, want) {
		t.Fatalf("live entries = %q", got)
	}
	c.replaceHistory()
	if got := warnTestEntries(c); !slices.Equal(got, want) {
		t.Fatalf("entries after history replacement = %q", got)
	}
}
