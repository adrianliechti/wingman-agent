package code

import "testing"

func TestActivateSessionResetsTurnState(t *testing.T) {
	a := &App{sessionID: "old", sessionEpoch: 3}
	a.phase.Store(int32(PhaseStreaming))
	a.streamingText = "partial"
	a.streamingReasoning = "thinking"
	a.currentToolName = "shell"
	a.reasoningID = "reasoning"

	a.activateSession("new")

	if a.sessionID != "new" || a.sessionEpoch != 4 {
		t.Fatalf("session = %q at epoch %d", a.sessionID, a.sessionEpoch)
	}
	if a.getPhase() != PhaseIdle {
		t.Fatalf("phase = %v", a.getPhase())
	}
	if a.streamingText != "" || a.streamingReasoning != "" || a.currentToolName != "" || a.reasoningID != "" {
		t.Fatalf("stream state was retained: text=%q reasoning=%q tool=%q id=%q",
			a.streamingText, a.streamingReasoning, a.currentToolName, a.reasoningID)
	}

	called := false
	a.withCurrentSession("old", func() { called = true })
	if called {
		t.Fatal("old session was still current")
	}
	a.withCurrentSession("new", func() { called = true })
	if !called {
		t.Fatal("new session was not current")
	}
}
