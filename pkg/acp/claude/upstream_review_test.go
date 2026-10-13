package claude

import (
	"context"
	"encoding/json"
	"io"
	"strings"
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

// Upstream claude-agent-acp 53df1c7: a cancel must end the tool calls it left
// without a result. A subagent's interrupted result is written only to its own
// transcript, and a dead or force-cancelled process reports nothing.
func TestCancelClosesToolCallsWithoutResults(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"root-bash","name":"Bash","input":{"command":"sleep 60"}},{"type":"tool_use","id":"agent-fg","name":"Agent","input":{}},{"type":"tool_use","id":"agent-bg","name":"Agent","input":{"run_in_background":true}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"task-fg","tool_use_id":"agent-fg"}`,
		`{"type":"system","subtype":"task_started","task_id":"task-bg","tool_use_id":"agent-bg","is_backgrounded":true}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"agent-bg","content":"started in the background"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"agent-fg","message":{"content":[{"type":"tool_use","id":"fg-bash","name":"Bash","input":{}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"agent-bg","message":{"content":[{"type":"tool_use","id":"bg-bash","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"root-bash","is_error":true,"content":"Interrupted"}]}}`,
	}, "\n") + "\n"
	for _, tt := range []struct {
		name         string
		processAlive bool
		want         map[string]string // closed tool call -> parent
	}{
		{"interrupted", true, map[string]string{"agent-fg": "", "fg-bash": "agent-fg"}},
		{"process gone", false, map[string]string{"agent-fg": "", "fg-bash": "agent-fg", "bg-bash": "agent-bg"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, conn, wire := newTranscriptProcess(t)
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				p.beginTurn(context.Background())
				go p.read(context.Background(), conn, "s", reader)
				_, _ = io.WriteString(writer, transcript)
				synctest.Wait()
				before := len(readTranscriptUpdates(t, wire.Bytes()))

				// session/cancel has already cancelled the prompt request context.
				promptCtx, cancelPrompt := context.WithCancel(context.Background())
				cancelPrompt()
				p.closeCancelledToolCalls(promptCtx, conn, "s", tt.processAlive)
				lateAssistant := `{"type":"assistant","parent_tool_use_id":"agent-fg","message":{"content":[{"type":"tool_use","id":"fg-bash","name":"Bash","input":{}}]}}`
				_, _ = io.WriteString(writer, lateAssistant+"\n")
				late := `{"type":"user","parent_tool_use_id":"agent-fg","message":{"content":[{"type":"tool_result","tool_use_id":"fg-bash","content":"late"}]}}`
				_, _ = io.WriteString(writer, late+"\n")
				synctest.Wait()

				closed := map[string]string{}
				for _, update := range readTranscriptUpdates(t, wire.Bytes())[before:] {
					u := update.ToolCallUpdate
					if u == nil {
						t.Fatalf("unexpected update after cancel: %+v", update)
					}
					if _, dup := closed[string(u.ToolCallId)]; dup {
						t.Fatalf("tool call %s updated twice after cancel", u.ToolCallId)
					}
					if u.Status == nil || *u.Status != acp.ToolCallStatusFailed {
						t.Fatalf("tool call %s status = %v, want failed", u.ToolCallId, u.Status)
					}
					parent := ""
					if meta, ok := u.Meta["claudeCode"].(map[string]any); ok {
						parent, _ = meta["parentToolUseId"].(string)
					}
					closed[string(u.ToolCallId)] = parent
				}
				if len(closed) != len(tt.want) {
					t.Fatalf("closed = %v, want %v", closed, tt.want)
				}
				for id, parent := range tt.want {
					if got, ok := closed[id]; !ok || got != parent {
						t.Fatalf("closed = %v, want %v", closed, tt.want)
					}
				}
			})
		})
	}
}

func TestCancelDoesNotFinalizePermissionDeniedCallAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, conn, wire := newTranscriptProcess(t)
		if err := emitAssistant(context.Background(), conn, "s", json.RawMessage(`{"content":[{"type":"tool_use","id":"denied","name":"Bash","input":{}}]}`), p.cwd, p.tools, p.emitted, p.streamedContent, ""); err != nil {
			t.Fatal(err)
		}
		p.handleSystem(context.Background(), conn, "s", cliEnvelope{Subtype: "permission_denied", ToolUseID: "denied", Message: json.RawMessage(`"blocked by policy"`)})
		p.closeCancelledToolCalls(context.Background(), conn, "s", true)
		if err := emitAssistant(context.Background(), conn, "s", json.RawMessage(`{"content":[{"type":"tool_use","id":"denied","name":"Bash","input":{}}]}`), p.cwd, p.tools, p.emitted, p.streamedContent, ""); err != nil {
			t.Fatal(err)
		}
		if err := emitToolResults(context.Background(), conn, "s", json.RawMessage(`{"content":[{"type":"tool_result","tool_use_id":"denied","is_error":true,"content":"late denial"}]}`), p.tools, p.emitted, ""); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		updates := readTranscriptUpdates(t, wire.Bytes())
		if len(updates) != 2 || updates[1].ToolCallUpdate == nil {
			t.Fatalf("updates = %+v, want one start and one denial", updates)
		}
		u := updates[1].ToolCallUpdate
		if u.Status == nil || *u.Status != acp.ToolCallStatusFailed || len(u.Content) != 1 || u.Content[0].Content == nil || u.Content[0].Content.Content.Text == nil || u.Content[0].Content.Content.Text.Text != "Permission denied: blocked by policy" {
			t.Fatalf("denial was replaced: %+v", u)
		}
	})
}

// Upstream claude-agent-acp b007ac7: after an interrupt, or while background
// agents run (CLI 2.1.270+), several results share one idle. The unpaid idle
// debt must not absorb a later abandoned turn's idle.
func TestRunningTransitionDropsUnpaidIdleDebt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, conn, _ := newTranscriptProcess(t)
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		go p.read(context.Background(), conn, "s", reader)
		for _, line := range []string{
			`{"type":"system","subtype":"session_state_changed","state":"running"}`,
			`{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}`,
			`{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}`,
			`{"type":"system","subtype":"session_state_changed","state":"idle"}`,
		} {
			_, _ = io.WriteString(writer, line+"\n")
		}
		synctest.Wait()
		p.beginTurn(context.Background())
		_, _ = io.WriteString(writer, `{"type":"system","subtype":"session_state_changed","state":"running"}`+"\n")
		_, _ = io.WriteString(writer, `{"type":"system","subtype":"session_state_changed","state":"idle"}`+"\n")
		synctest.Wait()
		select {
		case r := <-p.results:
			if r.err == nil {
				t.Fatalf("abandoned turn result = %+v, want an error", r)
			}
		default:
			t.Fatal("unpaid idle debt absorbed an abandoned turn's idle; the prompt would hang")
		}
	})
}

func TestRepeatedRunningKeepsOwedIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, conn, _ := newTranscriptProcess(t)
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		go p.read(context.Background(), conn, "s", reader)
		for _, line := range []string{
			`{"type":"system","subtype":"session_state_changed","state":"running"}`,
			`{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}`,
			`{"type":"system","subtype":"session_state_changed","state":"running"}`,
		} {
			_, _ = io.WriteString(writer, line+"\n")
		}
		synctest.Wait()
		p.beginTurn(context.Background())
		_, _ = io.WriteString(writer, `{"type":"system","subtype":"session_state_changed","state":"idle"}`+"\n")
		synctest.Wait()
		select {
		case r := <-p.results:
			t.Fatalf("an owed trailing idle ended the next turn: %+v", r)
		default:
		}
	})
}
