package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContextSectionsIncrementalDeltaAndCompactionExpansion(t *testing.T) {
	sections := []ContextSection{
		{Title: "Project Guidelines", Content: "Follow repository conventions."},
		{Title: "Skills", Content: strings.Repeat("skill-entry ", 200)},
		{Title: "Environment", Content: "- Current Date: 2026-11-12"},
		{Title: "Collaboration Mode", Content: "# Plan mode\nRead-only."},
	}
	a := &Agent{Config: &Config{
		ContextSections: func() []ContextSection { return sections },
	}}

	// Initial sync emits complete snapshot.
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 1 || !strings.HasPrefix(a.Messages[0].Content[0].Text, sessionContextPrefix) {
		t.Fatalf("initial message = %+v", a.Messages)
	}

	// No-op sync emits nothing.
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 1 {
		t.Fatalf("expected no duplicate snapshot, got %d messages", len(a.Messages))
	}

	// Update Environment and drop Collaboration Mode (Plan -> Agent).
	sections = []ContextSection{
		{Title: "Project Guidelines", Content: "Follow repository conventions."},
		{Title: "Skills", Content: strings.Repeat("skill-entry ", 200)},
		{Title: "Environment", Content: "- Current Date: 2026-11-13"},
	}
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(a.Messages))
	}
	delta := a.Messages[1].Content[0].Text
	if !strings.HasPrefix(delta, sessionContextDeltaPrefix) {
		t.Fatalf("expected incremental delta prefix, got:\n%s", delta)
	}
	if !strings.Contains(delta, "Omitted sections (no longer apply): Collaboration Mode.") {
		t.Fatalf("delta missing omitted section notice:\n%s", delta)
	}
	if !strings.Contains(delta, "2026-11-13") {
		t.Fatalf("delta missing updated Environment:\n%s", delta)
	}
	if strings.Contains(delta, "skill-entry") || strings.Contains(delta, "Follow repository conventions.") {
		t.Fatalf("delta resent unchanged sections:\n%s", delta)
	}

	// Expanding the delta for compaction/compression restores all active sections
	// without resurrecting removed sections.
	expanded := fullSessionContextMessage(a.Messages[1]).Content[0].Text
	if !strings.HasPrefix(expanded, sessionContextPrefix) {
		t.Fatalf("expanded message lacks full snapshot prefix:\n%s", expanded)
	}
	for _, want := range []string{"Follow repository conventions.", "skill-entry", "2026-11-13"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded snapshot lost %q", want)
		}
	}
	if strings.Contains(expanded, "Plan mode") {
		t.Errorf("expanded snapshot resurrected removed Collaboration Mode:\n%s", expanded)
	}
}

func TestEmptyContextDeltaExpandsAfterJSONRoundTrip(t *testing.T) {
	sections := []ContextSection{{Title: "Mode", Content: "Plan only"}}
	a := &Agent{Config: &Config{ContextSections: func() []ContextSection { return sections }}}
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	sections = nil
	if err := a.syncContextInstructions(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(a.Messages[1])
	if err != nil {
		t.Fatal(err)
	}
	var delta Message
	if err := json.Unmarshal(raw, &delta); err != nil {
		t.Fatal(err)
	}
	expanded := fullSessionContextMessage(delta)
	if got := contentText(expanded.Content); got != sessionContextPrefix {
		t.Fatalf("not a complete empty snapshot: %q", got)
	}
}
