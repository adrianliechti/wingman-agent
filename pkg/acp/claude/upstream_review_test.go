package claude

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/coder/acp-go-sdk"
)

func TestTaskUpdatesDoNotReferenceUnseenOrCompletedToolCalls(t *testing.T) {
	for _, tool := range []string{"Agent", "Bash", "Monitor"} {
		t.Run(tool, func(t *testing.T) {
			updates, _ := captureCLITranscript(t, context.Background(),
				`{"type":"system","subtype":"task_progress","task_id":"unknown","tool_use_id":"unseen","description":"unseen update"}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool","name":"`+tool+`","input":{}}]}}`,
				`{"type":"system","subtype":"task_started","task_id":"task","tool_use_id":"tool"}`,
				`{"type":"system","subtype":"task_progress","task_id":"task","description":"live progress"}`,
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool","content":"done"}]}}`,
				`{"type":"system","subtype":"task_progress","task_id":"task","description":"late progress"}`,
				`{"type":"system","subtype":"task_notification","task_id":"task","tool_use_id":"tool","summary":"late summary"}`,
				`{"type":"result","subtype":"success"}`,
			)
			seen, completed, progress := false, false, 0
			for _, update := range updates {
				if update.ToolCall != nil {
					seen = true
				}
				if u := update.ToolCallUpdate; u != nil {
					if !seen || completed {
						t.Fatalf("task updated an unseen or completed tool: %+v", u)
					}
					if u.Status != nil && *u.Status == acp.ToolCallStatusCompleted {
						completed = true
					} else {
						progress++
					}
				}
			}
			want := 1
			if tool == "Monitor" {
				want = 0
			}
			if progress != want {
				t.Fatalf("live task updates = %d, want %d", progress, want)
			}
		})
	}
}

func TestChildIdleCannotEndRootTurn(t *testing.T) {
	p := &claudeProc{results: make(chan turnResult, 1)}
	p.beginTurn(context.Background())
	defer p.finishTurn()
	p.handleSystem(context.Background(), nil, "s", cliEnvelope{Subtype: "session_state_changed", State: "idle", ParentAgentID: "child"})
	select {
	case r := <-p.results:
		t.Fatalf("child idle ended root turn: %+v", r)
	default:
	}
}

func TestCloseWaitsForActivePromptCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := New(Options{})
		s := a.newSession("s", t.TempDir(), "default", "", nil)
		a.storeSession(s)
		if err := s.promptMu.Lock(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		done := make(chan struct{})
		go func() {
			_, _ = a.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: s.id})
			close(done)
		}()
		<-ctx.Done()
		synctest.Wait()
		select {
		case <-done:
			t.Error("session close returned before active prompt cleanup")
		default:
		}
		s.promptMu.Unlock()
		<-done
	})
}
