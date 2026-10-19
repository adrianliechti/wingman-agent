package elicitation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/elicitation"
)

func TestMalformedQuestionsNeverReachUI(t *testing.T) {
	called := false
	inspect := elicitation.Tools(&tool.Elicitation{Elicit: func(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
		called = true
		return tool.ElicitResult{Action: tool.ElicitCancel}, nil
	}})[0]
	for name, entry := range map[string]map[string]any{
		"null header":         {"header": nil},
		"null multiple":       {"multi_select": nil},
		"null options":        {"options": nil},
		"empty options":       {"options": []any{}},
		"one option":          {"options": []any{map[string]any{"label": "One", "description": "Only"}}},
		"missing description": {"options": []any{map[string]any{"label": "One"}, map[string]any{"label": "Two", "description": "Two"}}},
		"invalid preview":     {"options": []any{map[string]any{"label": "One", "description": "One", "preview": true}, map[string]any{"label": "Two", "description": "Two"}}},
		"multi preview":       {"multi_select": true, "options": []any{map[string]any{"label": "One", "description": "One", "preview": "code"}, map[string]any{"label": "Two", "description": "Two"}}},
	} {
		t.Run(name, func(t *testing.T) {
			entry["question"] = "Which option?"
			if _, err := inspect.Execute(t.Context(), map[string]any{"questions": []any{entry}}); err == nil {
				t.Fatal("accepted malformed input")
			}
		})
	}
	var options []any
	for _, label := range []string{"A", "B", "C", "D", "E"} {
		options = append(options, map[string]any{"label": label, "description": label})
	}
	if _, err := inspect.Execute(t.Context(), map[string]any{"questions": []any{map[string]any{"question": "Which?", "options": options}}}); err == nil {
		t.Fatal("accepted too many options")
	}
	if called {
		t.Fatal("malformed questions reached UI")
	}
}

func TestElicitCancellationAndUnknownResponses(t *testing.T) {
	args := map[string]any{"questions": []any{map[string]any{"question": "Which?"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	inspect := elicitation.Tools(&tool.Elicitation{Elicit: func(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
		called = true
		return tool.ElicitResult{Action: "invalid"}, nil
	}})[0]
	if _, err := inspect.Execute(ctx, args); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled request reached UI: called=%v err=%v", called, err)
	}
	if _, err := inspect.Execute(t.Context(), args); err == nil {
		t.Fatal("invalid action reported as user dismissal")
	}
	ctx, cancel = context.WithCancel(t.Context())
	inspect = elicitation.Tools(&tool.Elicitation{Elicit: func(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
		cancel()
		return tool.ElicitResult{Action: tool.ElicitAccept, Content: map[string]any{"q_1": "answer"}}, nil
	}})[0]
	if _, err := inspect.Execute(ctx, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("response swallowed cancellation: %v", err)
	}
}
