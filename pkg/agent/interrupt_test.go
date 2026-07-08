package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func TestExecuteToolRendersInterruption(t *testing.T) {
	a := &Agent{Config: &Config{}}

	tl := &tool.Tool{
		Name: "probe",
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := a.executeTool(ctx, ToolCall{ID: "1", Name: "probe"}, tl, 0, time.Now())
	if !strings.Contains(got, "interrupted") {
		t.Fatalf("canceled call rendered as %q", got)
	}
}

func TestExecuteToolRendersTimeout(t *testing.T) {
	a := &Agent{Config: &Config{}}

	tl := &tool.Tool{
		Name: "probe",
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	timeout := 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	got := a.executeTool(ctx, ToolCall{ID: "1", Name: "probe"}, tl, timeout, time.Now())
	if !strings.Contains(got, "time limit") {
		t.Fatalf("timed-out call rendered as %q", got)
	}
}

func TestExecuteToolBlamesRequestDeadlineNotToolLimit(t *testing.T) {
	a := &Agent{Config: &Config{}}

	tl := &tool.Tool{
		Name: "probe",
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	// The parent deadline fires long before the tool's own generous limit.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	got := a.executeTool(ctx, ToolCall{ID: "1", Name: "probe"}, tl, time.Hour, time.Now())
	if !strings.Contains(got, "request deadline") {
		t.Fatalf("parent-deadline abort rendered as %q", got)
	}
}

func TestExecuteToolKeepsToolErrorWhenContextIntact(t *testing.T) {
	a := &Agent{Config: &Config{}}

	tl := &tool.Tool{
		Name: "probe",
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			return "", context.Canceled
		},
	}

	got := a.executeTool(context.Background(), ToolCall{ID: "1", Name: "probe"}, tl, 0, time.Now())
	if strings.Contains(got, "interrupted") {
		t.Fatalf("a tool's own canceled error must not read as user interruption: %q", got)
	}
}

func TestCutoffNoticeDefaultsReason(t *testing.T) {
	m := cutoffNotice("")
	if !m.Hidden || m.Role != RoleUser {
		t.Fatalf("notice must be a hidden user message, got %+v", m)
	}
	if !strings.Contains(m.Content[0].Text, "max_output_tokens") {
		t.Fatalf("notice missing default reason: %q", m.Content[0].Text)
	}
}
