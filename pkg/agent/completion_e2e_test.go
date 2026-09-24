package agent_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

// Exercise HTTP streaming, real side effects, the journal, and process-style
// restoration. A transport's completed response must not imply a completed task.
func TestCompletionE2EPreservesWorkAndResumes(t *testing.T) {
	for _, requireFinish := range []bool{true, false} {
		t.Run(fmt.Sprintf("require_finish=%t", requireFinish), func(t *testing.T) {
			var requests, executions atomic.Int64
			path := filepath.Join(t.TempDir(), "receipt.txt")
			provider := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
				n := requests.Add(1)
				switch {
				case n == 1:
					fmt.Fprint(w, completedToolCall("record-once", "record"))
				case n <= 4:
					body := completedText("Now I will verify the receipt.")
					if !requireFinish {
						body = strings.ReplaceAll(body, `"role":"assistant"`, `"role":"assistant","phase":"commentary"`)
					}
					fmt.Fprint(w, body)
				case n == 5:
					if !bytes.Contains(req.Input, []byte("receipt=recorded-once")) || !bytes.Contains(req.Input, []byte("Continue")) {
						t.Error("resume lost completed work or fresh user input")
					}
					fmt.Fprint(w, completedText("Verified receipt=recorded-once."))
				case n == 6 && requireFinish:
					fmt.Fprint(w, completedToolCall("finished", "finish_turn"))
				default:
					t.Error("unexpected extra request")
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			})
			cfg := e2eConfig(t, tool.Tool{Name: "record", Execute: func(context.Context, map[string]any) (tool.Result, error) {
				executions.Add(1)
				if err := os.WriteFile(path, []byte("recorded-once"), 0600); err != nil {
					return tool.Result{}, err
				}
				return tool.Text("receipt=recorded-once"), nil
			}})
			cfg.RequireFinish = func(string) bool { return requireFinish }
			cfg.MaxTurns = 10
			dir := t.TempDir()
			journal, err := session.OpenJournal(dir, "completion")
			if err != nil {
				t.Fatal(err)
			}
			a := &agent.Agent{Config: cfg, Recorder: journal}
			if err := drain(t, a, "Record a receipt and verify it."); !errors.Is(err, agent.ErrTurnIncomplete) {
				t.Fatalf("first turn outcome: %v", err)
			}
			if requests.Load() != 4 || executions.Load() != 1 {
				t.Fatalf("requests=%d tool executions=%d", requests.Load(), executions.Load())
			}
			loaded, err := session.Load(dir, "completion")
			if err != nil {
				t.Fatal(err)
			}
			reviews := agent.BuildTurnReviews(loaded.State.Events)
			if len(reviews) != 1 || reviews[0].Outcome != "incomplete" {
				t.Fatalf("saved reviews: %+v", reviews)
			}
			a = &agent.Agent{Config: cfg, Recorder: journal}
			if err := a.Restore(loaded.State); err != nil {
				t.Fatal(err)
			}
			if err := drain(t, a, "Continue; use the saved receipt and do not record it again."); err != nil {
				t.Fatal(err)
			}
			wantRequests := int64(5)
			if requireFinish {
				wantRequests++
			}
			if requests.Load() != wantRequests || executions.Load() != 1 {
				t.Fatalf("resume requests=%d executions=%d", requests.Load(), executions.Load())
			}
			if receipt, err := os.ReadFile(path); err != nil || string(receipt) != "recorded-once" {
				t.Fatalf("receipt=%q error=%v", receipt, err)
			}
			reviews = a.TurnReviews()
			if len(reviews) != 2 || reviews[0].Outcome != "incomplete" || reviews[1].Outcome != "completed" {
				t.Fatalf("resumed reviews: %+v", reviews)
			}
			// New rounds append to the cached history, including across resume.
			all := provider.snapshot()
			for i := 1; i < len(all); i++ {
				assertRequestPrefix(t, all[i-1], all[i])
			}
		})
	}
}
