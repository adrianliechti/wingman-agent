package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func yieldAll(Message, error) bool { return true }

func TestProcessToolCallsRunsReadsConcurrently(t *testing.T) {
	var mu sync.Mutex
	started := 0
	release := make(chan struct{})

	readExec := func(ctx context.Context, args map[string]any) (string, error) {
		mu.Lock()
		started++
		if started == 2 {
			close(release)
		}
		mu.Unlock()

		select {
		case <-release:
			return "ok", nil
		case <-ctx.Done():
			return "", ctx.Err()
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

	rec := func(name string) func(context.Context, map[string]any) (string, error) {
		return func(context.Context, map[string]any) (string, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return "ok", nil
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
