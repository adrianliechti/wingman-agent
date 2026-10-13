package claude

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/coder/acp-go-sdk"
)

func TestCancelIgnoresLateAssistantAfterEagerToolStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, conn, wire := newTranscriptProcess(t)
		// A permission request announced this call before its assistant block.
		if err := p.emitted.emit("tool", "Bash", "", func() error {
			return emitToolUseCall(context.Background(), conn, "s", cliMsgBlock{ID: "tool", Name: "Bash", Input: json.RawMessage(`{}`)}, p.cwd, nil, "")
		}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		p.closeCancelledToolCalls(context.Background(), conn, "s", true)
		if err := emitAssistant(context.Background(), conn, "s", json.RawMessage(`{"content":[{"type":"tool_use","id":"tool","name":"Bash","input":{}}]}`), p.cwd, p.tools, p.emitted, p.streamedContent, ""); err != nil {
			t.Fatal(err)
		}
		if err := emitToolResults(context.Background(), conn, "s", json.RawMessage(`{"content":[{"type":"tool_result","tool_use_id":"tool","content":"late result"}]}`), p.tools, p.emitted, ""); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		updates := readTranscriptUpdates(t, wire.Bytes())
		if len(updates) != 2 || updates[0].ToolCall == nil || updates[1].ToolCallUpdate == nil || updates[1].ToolCallUpdate.Status == nil || *updates[1].ToolCallUpdate.Status != acp.ToolCallStatusFailed {
			t.Fatalf("updates = %+v, want one start and one cancellation failure", updates)
		}
	})
}

type cancellationWriterFunc func([]byte) (int, error)

func (w cancellationWriterFunc) Write(b []byte) (int, error) { return w(b) }

func TestCancelClosesBackgroundCallsWhenResultAndEOFReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Both select cases are ready. Every choice must end the call, including
		// when the result wins and EOF would previously have been overlooked.
		for i := range 40 {
			p, conn, wire := newTranscriptProcess(t)
			s := p.session
			s.agent.SetAgentConnection(conn)
			s.proc = p
			p.sig = s.spawnSigLocked()
			p.subagentParents["task"] = "agent-bg"
			p.backgroundTasks = map[string]bool{"task": true}
			if err := emitToolUseCall(context.Background(), conn, "s", cliMsgBlock{ID: "bg-bash", Name: "Bash", Input: json.RawMessage(`{}`)}, p.cwd, p.emitted, "agent-bg"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			written := false
			p.out = &streamWriter{w: cancellationWriterFunc(func(b []byte) (int, error) {
				if !written {
					written = true
					cancel()
					p.finishTurn()
					p.results <- turnResult{stop: acp.StopReasonCancelled}
					close(p.dead)
				}
				return len(b), nil
			})}
			stop, _, err := s.runTurn(ctx, []acp.ContentBlock{acp.TextBlock("hello")})
			cancel()
			if err != nil || stop != acp.StopReasonCancelled {
				t.Fatalf("iteration %d: stop = %q, error = %v", i, stop, err)
			}
			synctest.Wait()
			updates := readTranscriptUpdates(t, wire.Bytes())
			if p.emitted.has("bg-bash") || len(updates) != 2 || updates[1].ToolCallUpdate == nil || updates[1].ToolCallUpdate.Status == nil || *updates[1].ToolCallUpdate.Status != acp.ToolCallStatusFailed {
				t.Fatalf("iteration %d: background call did not end exactly once: %+v", i, updates)
			}
		}
	})
}

func TestCancelClosesBackgroundCallsDuringSessionClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, conn, wire := newTranscriptProcess(t)
		p.subagentParents["task"] = "agent-bg"
		p.backgroundTasks = map[string]bool{"task": true}
		if err := emitToolUseCall(context.Background(), conn, "s", cliMsgBlock{ID: "bg-bash", Name: "Bash", Input: json.RawMessage(`{}`)}, p.cwd, p.emitted, "agent-bg"); err != nil {
			t.Fatal(err)
		}
		p.session.mu.Lock()
		p.session.closed = true
		p.session.mu.Unlock()
		// The process has not reported EOF yet, but session close is stopping it.
		p.closeCancelledToolCalls(context.Background(), conn, "s", true)
		synctest.Wait()
		updates := readTranscriptUpdates(t, wire.Bytes())
		if p.emitted.has("bg-bash") || len(updates) != 2 || updates[1].ToolCallUpdate == nil {
			t.Fatalf("closing session kept a background call open: %+v", updates)
		}
	})
}
