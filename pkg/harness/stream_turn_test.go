package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func endTurnResponse(endTurn string, items ...string) string {
	return strings.Replace(phaseTestResponse(false, items...), `"response":{`, `"response":{"end_turn":`+endTurn+`,`, 1)
}

func TestFailedStreamDoesNotRetryAfterPersistenceFailure(t *testing.T) {
	for _, failure := range []string{"run terminal", "tool result"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				client := streamingTestClient(func(*http.Request) string {
					requests++
					if requests > 1 {
						return phaseTestResponse(false, finalAnswerOutput)
					}
					output := ""
					if failure == "tool result" {
						output = itemDoneEvents(loopCall("write", "write", `{}`))
					}
					return output + "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"retry\"}}}\n\n"
				})
				storageErr := errors.New("storage unavailable")
				a := &Agent{Config: &Config{client: &client, MaxTurns: 2, Tools: func() []tool.Tool {
					return []tool.Tool{{Name: "write", Execute: func(context.Context, map[string]any) (tool.Result, error) {
						return tool.Text("written"), nil
					}}}
				}}, Recorder: EventRecorderFunc(func(events []RuntimeEvent) error {
					for _, event := range events {
						if requests != 1 {
							continue
						}
						if failure == "run terminal" && event.Type == EventRunTerminal {
							return storageErr
						}
						if failure == "tool result" && event.Message != nil && len(event.Message.Content) > 0 && event.Message.Content[0].ToolResult != nil {
							return storageErr
						}
					}
					return nil
				})}
				ctx := WithStreamEventHandlers(t.Context(), StreamEventHandlers{Reset: func() {}})
				err := runLoopTurn(ctx, a, "work")
				if requests != 1 || !errors.Is(err, storageErr) {
					t.Fatalf("requests=%d error=%v; want no retry after storage failure", requests, err)
				}
			})
		})
	}
}

func TestSendHonorsExplicitTurnBoundary(t *testing.T) {
	for _, requireFinish := range []bool{false, true} {
		for _, tc := range []struct{ name, output string }{
			{"reasoning", `{"type":"reasoning","id":"rs_probe","summary":[{"type":"summary_text","text":"Still checking."}]}`},
			{"commentary", commentaryOutput},
			{"final answer", finalAnswerOutput},
		} {
			t.Run(fmt.Sprintf("%s/require_finish=%t", tc.name, requireFinish), func(t *testing.T) {
				requests := 0
				client := streamingTestClient(func(r *http.Request) string {
					requests++
					req := readLoopRequest(t, r)
					for _, item := range req.Input[1:] {
						if item.Role == "user" {
							t.Error("native continuation inserted a user reminder")
						}
					}
					if requests <= 4 {
						return endTurnResponse("false", tc.output)
					}
					// The explicit boundary takes precedence over phase-based recovery.
					return endTurnResponse("true", commentaryOutput)
				})
				a := &Agent{Config: &Config{client: &client, MaxTurns: 6, RequireFinish: func(string) bool { return requireFinish }}}
				if err := runLoopTurn(t.Context(), a, "work"); err != nil {
					t.Fatal(err)
				}
				if requests != 5 {
					t.Fatalf("requests=%d, want four continuations followed by completion", requests)
				}
			})
		}
	}
}

func TestExplicitContinuationRespectsTurnLimit(t *testing.T) {
	requests := 0
	client := streamingTestClient(func(*http.Request) string {
		requests++
		return endTurnResponse("false")
	})
	a := &Agent{Config: &Config{client: &client, MaxTurns: 2}}
	if err := runLoopTurn(t.Context(), a, "work"); !errors.Is(err, ErrMaxTurnsExceeded) || requests != 2 {
		t.Fatalf("error=%v requests=%d", err, requests)
	}
}

func TestReasoningReplayAcrossStreamRetry(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native_continuation=%t", native), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const reasoning = `{"type":"reasoning","id":"rs_probe","encrypted_content":"opaque","summary":[]}`
				const failed = "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"retry\"}}}\n\n"
				requests := 0
				client := streamingTestClient(func(r *http.Request) string {
					requests++
					var req struct {
						Input []struct {
							Type             string `json:"type"`
							EncryptedContent string `json:"encrypted_content"`
						} `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					if requests == 1 {
						if native {
							return endTurnResponse("false", reasoning)
						}
						return itemDoneEvents(reasoning) + failed
					}
					replayed := false
					for _, item := range req.Input {
						replayed = replayed || item.Type == "reasoning" && item.EncryptedContent == "opaque"
					}
					if replayed != native {
						t.Errorf("request %d reasoning replay=%t, want %t", requests, replayed, native)
					}
					if native && requests == 2 {
						return failed
					}
					return endTurnResponse("true", finalAnswerOutput)
				})
				a := &Agent{Config: &Config{client: &client, MaxTurns: 2}}
				if err := runLoopTurn(t.Context(), a, "work"); err != nil {
					t.Fatal(err)
				}
				wantRequests := 2
				if native {
					wantRequests++
				}
				if requests != wantRequests {
					t.Fatalf("requests=%d, want %d", requests, wantRequests)
				}
			})
		})
	}
}

func TestFailedStreamRejectsRetainedFinishMarker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		client := streamingTestClient(func(r *http.Request) string {
			requests++
			if requests == 1 {
				return itemDoneEvents(loopCall("finish", finishToolName, `{"answer":"done"}`)) +
					"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"retry\"}}}\n\n"
			}
			req := readLoopRequest(t, r)
			want := []string{"user:work", "call:finish", "result:finish"}
			if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
				t.Errorf("retry input=%v, want %v", got, want)
			}
			return phaseTestResponse(false, loopCall("finish_retry", finishToolName, `{"answer":"done"}`))
		})
		a := &Agent{Config: &Config{client: &client, RequireFinish: func(string) bool { return true }}}
		if err := runLoopTurn(t.Context(), a, "work"); err != nil {
			t.Fatal(err)
		}
		if requests != 2 {
			t.Fatalf("requests=%d", requests)
		}
	})
}

// The first response withholds its terminal event until release. This lets
// tests observe actual execution while the model stream is still open.
func heldResponseClient(prefix, suffix string, release <-chan struct{}, followup func(*http.Request) string) openai.Client {
	requests := 0
	return openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			var body io.ReadCloser
			if requests == 1 {
				reader, writer := io.Pipe()
				body = reader
				go func() {
					defer writer.Close()
					stop := context.AfterFunc(r.Context(), func() { _ = writer.CloseWithError(r.Context().Err()) })
					defer stop()
					if _, err := io.WriteString(writer, prefix); err != nil {
						return
					}
					select {
					case <-release:
						_, _ = io.WriteString(writer, suffix)
					case <-r.Context().Done():
					}
				}()
			} else {
				body = io.NopCloser(strings.NewReader(followup(r)))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
		}),
	}))
}

func itemDoneEvents(items ...string) string {
	stream := phaseTestResponse(true, items...)
	return stream[:strings.Index(stream, "data: {\"type\":\"response.completed\"")]
}

func TestToolsStartBeforeResponseCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		call := loopCall("write", "probe", `{}`)
		followups := 0
		client := heldResponseClient(itemDoneEvents(call), endTurnResponse("true", call), release, func(r *http.Request) string {
			followups++
			req := readLoopRequest(t, r)
			want := []string{"user:work", "call:write", "result:write"}
			if got := loopInputOrder(req.Input); !slices.Equal(got, want) {
				t.Errorf("follow-up input=%v, want %v", got, want)
			}
			return endTurnResponse("true", finalAnswerOutput)
		})
		var executions atomic.Int64
		a := &Agent{Config: &Config{client: &client, MaxTurns: 2}}
		a.Tools = func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Execute: func(context.Context, map[string]any) (tool.Result, error) {
				if calls := extractToolCalls(a.MessagesSnapshot()); len(calls) != 1 || calls[0].ID != "write" {
					t.Error("tool began before its call was persisted")
				}
				executions.Add(1)
				return tool.Text("written"), nil
			}}}
		}
		done := make(chan error, 1)
		go func() { done <- runLoopTurn(t.Context(), a, "work") }()
		synctest.Wait()
		if executions.Load() != 1 {
			t.Error("tool did not execute while the response was open")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if executions.Load() != 1 || followups != 1 {
			t.Fatalf("executions=%d followups=%d; terminal output must not dispatch twice or end before tool follow-up", executions.Load(), followups)
		}
	})
}

func TestStreamingToolsPreserveReadWriteBarriers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		releaseResponse, releaseReads, releaseWrite := make(chan struct{}), make(chan struct{}), make(chan struct{})
		calls := []string{
			loopCall("r1", "probe", `{"kind":"read"}`), loopCall("r2", "probe", `{"kind":"read"}`),
			loopCall("w", "probe", `{"kind":"write"}`), loopCall("r3", "probe", `{"kind":"last"}`),
		}
		client := heldResponseClient(itemDoneEvents(calls...), phaseTestResponse(false, calls...), releaseResponse, func(*http.Request) string {
			return phaseTestResponse(false, finalAnswerOutput)
		})
		var reads, writes, last atomic.Int64
		a := &Agent{Config: &Config{client: &client, MaxParallelTools: 2, Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Effect: func(args map[string]any) tool.Effect {
				if args["kind"] == "write" {
					return tool.EffectMutates
				}
				return tool.EffectReadOnly
			}, Execute: func(_ context.Context, args map[string]any) (tool.Result, error) {
				switch args["kind"] {
				case "read":
					reads.Add(1)
					<-releaseReads
				case "write":
					writes.Add(1)
					<-releaseWrite
				case "last":
					last.Add(1)
				}
				return tool.Text("done"), nil
			}}}
		}}}
		done := make(chan error, 1)
		go func() { done <- runLoopTurn(t.Context(), a, "work") }()
		synctest.Wait()
		if reads.Load() != 2 || writes.Load() != 0 || last.Load() != 0 {
			t.Error("reads did not overlap or a mutation overtook them")
		}
		close(releaseReads)
		synctest.Wait()
		if writes.Load() != 1 || last.Load() != 0 {
			t.Error("mutation did not retain its exclusive position")
		}
		close(releaseWrite)
		synctest.Wait()
		if last.Load() != 1 {
			t.Error("read after mutation did not start during streaming")
		}
		close(releaseResponse)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestStreamingCancellationDrainsStartedTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := heldResponseClient(itemDoneEvents(loopCall("read", "probe", `{}`)), "", make(chan struct{}), func(*http.Request) string {
			t.Error("canceled stream retried")
			return phaseTestResponse(false, finalAnswerOutput)
		})
		var started, cleaned atomic.Bool
		a := &Agent{Config: &Config{client: &client, Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
				started.Store(true)
				<-ctx.Done()
				time.Sleep(time.Second)
				cleaned.Store(true)
				return tool.Result{}, ctx.Err()
			}}}
		}}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runLoopTurn(ctx, a, "work") }()
		synctest.Wait()
		if !started.Load() {
			t.Error("tool did not start before cancellation")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
		if !cleaned.Load() || a.Running() {
			t.Error("turn ended before tool cleanup")
		}
		var results int
		for _, m := range a.MessagesSnapshot() {
			for _, c := range m.Content {
				if c.ToolResult != nil {
					results++
					if c.ToolResult.Content != interruptedToolResult {
						t.Errorf("interrupted result=%s", c.ToolResult.Content)
					}
				}
			}
		}
		if results != 1 {
			t.Fatalf("interrupted tool results=%d", results)
		}
	})
}

func TestStreamingDuplicateCallRunsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		client := streamingTestClient(func(*http.Request) string {
			requests++
			if requests == 1 {
				call := loopCall("duplicate", "probe", `{}`)
				return phaseTestResponse(true, call, call)
			}
			return phaseTestResponse(false, finalAnswerOutput)
		})
		var executions atomic.Int64
		a := &Agent{Config: &Config{client: &client, Tools: func() []tool.Tool {
			return []tool.Tool{{Name: "probe", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: func(context.Context, map[string]any) (tool.Result, error) {
				executions.Add(1)
				time.Sleep(time.Second)
				return tool.Text("done"), nil
			}}}
		}}}
		if err := runLoopTurn(t.Context(), a, "work"); err != nil {
			t.Fatal(err)
		}
		if executions.Load() != 1 || requests != 2 {
			t.Fatalf("executions=%d requests=%d", executions.Load(), requests)
		}
	})
}
