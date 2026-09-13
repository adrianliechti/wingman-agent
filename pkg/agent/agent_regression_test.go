package agent

import (
	"context"
	"errors"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"net/http"
	"testing"
)

func TestRecorderFailureAfterConsumerBreakDoesNotYieldAgain(t *testing.T) {
	client := streamingTestClient(func(*http.Request) string {
		return "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_final\",\"delta\":\"Checked\"}\n\n" + phaseTestResponse(false, finalAnswerOutput)
	})
	a := &Agent{Config: &Config{client: &client}, Recorder: EventRecorderFunc(func(events []RuntimeEvent) error {
		for _, e := range events {
			if e.Type == EventRunTerminal {
				return errors.New("simulated recorder failure")
			}
		}
		return nil
	})}
	stream, err := a.Send(t.Context(), []Content{{Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}

	for range stream {
		break
	}
	if a.Running() {
		t.Fatal("agent remained busy after consumer closed")
	}
}

func TestCompletedEditMetadataSurvivesPostToolFailure(t *testing.T) {
	a := &Agent{Config: &Config{Hooks: hook.Hooks{PostToolUse: []hook.PostToolUse{func(context.Context, tool.ToolCall, string) (hook.Outcome, error) {
		return hook.Outcome{}, errors.New("hook failed")
	}}}}}
	result := a.runSingleToolCall(t.Context(), ToolCall{Name: "edit"}, []tool.Tool{{Name: "edit", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Result{Content: "edited", Metadata: map[string]any{tool.FileChangesMetadata: []tool.FileChange{{Path: "file", Before: "user", After: "agent"}}}}, nil
	}}})
	changes, err := tool.ResultFileChanges(result.Metadata)
	if !result.IsError || err != nil || len(changes) != 1 || changes[0].Before != "user" {
		t.Fatalf("lost completed edit: %+v, %v", result, err)
	}
}
