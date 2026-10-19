package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func yieldAll(Message, error) bool { return true }

func TestParallelToolResultsKeepCallOrderWhenRecorded(t *testing.T) {
	for _, rejectBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject_batch=%t", rejectBatch), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first, second := make(chan struct{}), make(chan struct{})
				started := make(chan struct{}, 2)
				tools := []tool.Tool{{
					Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly),
					Execute: func(_ context.Context, args map[string]any) (tool.Result, error) {
						started <- struct{}{}
						id := args["id"].(string)
						if id == "first" {
							<-first
						} else {
							<-second
						}
						return tool.Text(id), nil
					},
				}}
				var recorded []string
				rejected := false
				a := &Agent{Config: &Config{ToolTimeout: -1}, Recorder: EventRecorderFunc(func(events []RuntimeEvent) error {
					var ids []string
					for _, event := range events {
						if event.Type == EventToolTerminal {
							ids = append(ids, event.Tool.CallID)
						}
					}
					if rejectBatch && len(ids) > 1 {
						rejected = true
						return errors.New("batch rejected")
					}
					recorded = append(recorded, ids...)
					return nil
				})}
				var visible []string
				done := make(chan error, 1)
				go func() {
					done <- a.processToolCalls(t.Context(), []ToolCall{
						{ID: "first", Name: "read", Args: `{"id":"first"}`},
						{ID: "second", Name: "read", Args: `{"id":"second"}`},
					}, tools, func(m Message, _ error) bool {
						if result := m.Content[0].ToolResult; result != nil {
							if result.IsError || result.Content != result.ID {
								t.Errorf("result lost during persistence: %+v", result)
							}
							visible = append(visible, result.ID)
						}
						return true
					})
				}()
				<-started
				<-started
				close(second)
				synctest.Wait()
				close(first)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if len(started) != 0 {
					t.Fatal("persistence recovery executed tools again")
				}
				if rejected != rejectBatch || !slices.Equal(recorded, []string{"first", "second"}) {
					t.Fatalf("rejected=%t recorded=%v; results must be recorded once in call order", rejected, recorded)
				}
				if !slices.Equal(visible, []string{"second", "first"}) {
					t.Fatalf("visible=%v; results must be delivered in completion order", visible)
				}
			})
		})
	}
}

func TestProcessToolCallsRunsReadsConcurrently(t *testing.T) {
	var mu sync.Mutex
	started := 0
	release := make(chan struct{})

	readExec := func(ctx context.Context, args map[string]any) (tool.Result, error) {
		mu.Lock()
		started++
		if started == 2 {
			close(release)
		}
		mu.Unlock()

		select {
		case <-release:
			return tool.Text("ok"), nil
		case <-ctx.Done():
			return tool.Result{}, ctx.Err()
		}
	}

	tools := []tool.Tool{
		{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: readExec},
	}

	a := &Agent{Config: &Config{ToolTimeout: 2 * time.Second}}
	calls := []ToolCall{{ID: "1", Name: "read"}, {ID: "2", Name: "read"}}

	if err := a.processToolCalls(context.Background(), calls, tools, yieldAll); err != nil {
		t.Fatal(err)
	}

	for _, m := range a.Messages {
		for _, c := range m.Content {
			if c.ToolResult != nil && strings.HasPrefix(c.ToolResult.Content, "error") {
				t.Fatalf("reads did not overlap: %s", c.ToolResult.Content)
			}
		}
	}
}

func TestProcessToolCallsOrdersMixedSegments(t *testing.T) {
	var mu sync.Mutex
	var order []string

	rec := func(name string) func(context.Context, map[string]any) (tool.Result, error) {
		return func(context.Context, map[string]any) (tool.Result, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return tool.Text("ok"), nil
		}
	}

	tools := []tool.Tool{
		{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly), Execute: rec("read")},
		{Name: "write", Effect: tool.StaticEffect(tool.EffectMutates), Execute: rec("write")},
	}

	a := &Agent{Config: &Config{}}
	calls := []ToolCall{
		{ID: "1", Name: "read"},
		{ID: "2", Name: "read"},
		{ID: "3", Name: "write"},
		{ID: "4", Name: "read"},
	}

	if err := a.processToolCalls(context.Background(), calls, tools, yieldAll); err != nil {
		t.Fatal(err)
	}

	if len(order) != 4 || order[2] != "write" || order[0] != "read" || order[1] != "read" || order[3] != "read" {
		t.Fatalf("expected [read read write read] segments, got %v", order)
	}

	var ids []string
	for _, m := range a.Messages {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				ids = append(ids, c.ToolResult.ID)
			}
		}
	}
	if want := []string{"1", "2", "3", "4"}; !slices.Equal(ids, want) {
		t.Fatalf("result message order %v, want %v", ids, want)
	}
}

func TestDuplicateToolCallIDExecutesOnce(t *testing.T) {
	var calls int
	release := make(chan struct{})
	started := make(chan struct{})
	tl := tool.Tool{
		Name:   "write",
		Effect: tool.StaticEffect(tool.EffectMutates),
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			calls++
			close(started)
			<-release
			return tool.Text("written"), nil
		},
	}
	a := &Agent{Config: &Config{ToolTimeout: -1}}
	tc := ToolCall{ID: "same-call", Name: "write", Args: `{"path":"a"}`}

	results := make(chan tool.Result, 2)
	go func() { results <- a.runSingleToolCall(context.Background(), tc, []tool.Tool{tl}) }()
	<-started
	go func() { results <- a.runSingleToolCall(context.Background(), tc, []tool.Tool{tl}) }()
	close(release)

	first, second := <-results, <-results
	if calls != 1 || first.Content != "written" || second.Content != "written" {
		t.Fatalf("calls=%d results=(%+v, %+v)", calls, first, second)
	}
}

func TestCanceledDuplicateWaiterLeavesToolAndOtherWaitersRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		calls := 0
		tools := []tool.Tool{{
			Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly),
			Execute: func(context.Context, map[string]any) (tool.Result, error) {
				calls++
				close(started)
				<-release
				return tool.Result{Content: "read", Metadata: map[string]any{"version": 1}}, nil
			},
		}}
		a := &Agent{Config: &Config{ToolTimeout: -1}}
		call := ToolCall{ID: "shared", Name: "read", Args: `{}`}
		results := make(chan tool.Result, 2)
		go func() { results <- a.runSingleToolCallOutcomeDeferred(t.Context(), call, tools).result }()
		<-started

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		canceled := make(chan tool.Result, 1)
		go func() { canceled <- a.runSingleToolCallOutcomeDeferred(ctx, call, tools).result }()
		go func() { results <- a.runSingleToolCallOutcomeDeferred(t.Context(), call, tools).result }()
		synctest.Wait()
		cancel()
		if result := <-canceled; !result.IsError || result.Content != interruptedWaitResult {
			t.Fatalf("canceled waiter = %+v", result)
		}
		select {
		case result := <-results:
			t.Fatalf("canceling a waiter finished another call: %+v", result)
		default:
		}

		close(release)
		first, second := <-results, <-results
		if calls != 1 || first.IsError || second.IsError || first.Content != "read" || second.Content != "read" {
			t.Fatalf("calls=%d, results=(%+v, %+v)", calls, first, second)
		}
		first.Metadata["version"] = 2
		if second.Metadata["version"] != 1 {
			t.Fatal("waiters share mutable result metadata")
		}
		second.Metadata["version"] = 3
		cached := a.runSingleToolCallOutcomeDeferred(t.Context(), call, tools).result
		if cached.Metadata["version"] != 1 || calls != 1 {
			t.Fatalf("cached result changed or executed again: result=%+v calls=%d", cached, calls)
		}
	})
}

func TestRetainedToolResultPreventsReplayAfterRestore(t *testing.T) {
	a := &Agent{
		Config: &Config{ToolTimeout: -1},
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{{ToolCall: &ToolCall{
				ID: "persisted", Name: "write", Args: `{"path":"a"}`,
			}}}},
			{Role: RoleAssistant, Content: []Content{{ToolResult: &ToolResult{
				ID: "persisted", Name: "write", Args: `{"path":"a"}`, Content: "already written",
			}}}},
		},
	}
	called := false
	tl := tool.Tool{Name: "write", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		called = true
		return tool.Text("replayed"), nil
	}}

	if err := a.processToolCalls(context.Background(), []ToolCall{{
		ID: "persisted", Name: "write", Args: `{"path":"a"}`,
	}}, []tool.Tool{tl}, yieldAll); err != nil {
		t.Fatal(err)
	}
	result := a.Messages[len(a.Messages)-1].Content[0].ToolResult
	if called || result == nil || result.Content != "already written" {
		t.Fatalf("called=%v result=%+v", called, result)
	}
}

func TestRetainedImageResultRestoresAttachedData(t *testing.T) {
	a := &Agent{
		Config: &Config{ToolTimeout: -1},
		Messages: []Message{{Role: RoleAssistant, Content: []Content{
			{ToolResult: &ToolResult{ID: "image", Name: "view_image", Content: "[image attached below]"}},
			{File: &File{Data: "data:image/png;base64,abc"}},
		}}},
	}

	result := a.runSingleToolCall(context.Background(), ToolCall{ID: "image", Name: "view_image"}, nil)
	if result.Content != "data:image/png;base64,abc" {
		t.Fatalf("restored image = %q", result.Content)
	}
}

func TestToolResultMessagePreservesStructuredResult(t *testing.T) {
	message := toolResultMessage(ToolCall{ID: "call", Name: "shell"}, tool.Result{
		Content: "failed", IsError: true, Metadata: map[string]any{"exit_code": 7},
	})
	result := message.Content[0].ToolResult
	if result == nil || !result.IsError || result.Content != "failed" || result.Metadata["exit_code"] != 7 {
		t.Fatalf("tool result = %+v", result)
	}
}

func TestToolCallIDReuseWithDifferentArgumentsFails(t *testing.T) {
	a := &Agent{Config: &Config{ToolTimeout: -1}}
	tl := tool.Tool{Name: "write", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Text("ok"), nil
	}}
	tools := []tool.Tool{tl}
	first := a.runSingleToolCall(context.Background(), ToolCall{ID: "reused", Name: "write", Args: `{"v":1}`}, tools)
	second := a.runSingleToolCall(context.Background(), ToolCall{ID: "reused", Name: "write", Args: `{"v":2}`}, tools)
	if first.IsError || !second.IsError || !strings.Contains(second.Content, "reused") {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}
