package ask_test

import (
	"context"
	"errors"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	. "github.com/adrianliechti/wingman-agent/pkg/agent/tool/ask"
)

func TestToolsReturnsNilWithoutElicitation(t *testing.T) {
	if got := Tools(nil); got != nil {
		t.Fatalf("Tools(nil) = %v, want nil", got)
	}

	if got := Tools(&tool.Elicitation{}); got != nil {
		t.Fatalf("Tools with nil Ask = %v, want nil", got)
	}
}

func TestToolsReturnsHiddenAskUserTool(t *testing.T) {
	elicit := &tool.Elicitation{
		Ask: func(context.Context, string) (string, error) { return "", nil },
	}

	tools := Tools(elicit)
	if len(tools) != 1 {
		t.Fatalf("len(Tools) = %d, want 1", len(tools))
	}

	askTool := tools[0]
	if askTool.Name != "ask_user" {
		t.Errorf("name = %q, want ask_user", askTool.Name)
	}
	if !askTool.Hidden {
		t.Error("ask_user must be Hidden so the model treats it as elicitation, not a user-visible tool")
	}
	if askTool.Effect == nil || askTool.Effect(nil) != tool.EffectReadOnly {
		t.Error("ask_user effect must classify as EffectReadOnly")
	}

	required, ok := askTool.Parameters["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "question" {
		t.Errorf("required = %v, want [question]", askTool.Parameters["required"])
	}
}

func TestAskExecuteRejectsMissingOrEmptyQuestion(t *testing.T) {
	elicit := &tool.Elicitation{
		Ask: func(context.Context, string) (string, error) {
			t.Fatal("Ask must not be called when validation fails")
			return "", nil
		},
	}
	askTool := Tools(elicit)[0]

	cases := []map[string]any{
		{},
		{"question": ""},
		{"question": 42}, // wrong type
	}
	for _, args := range cases {
		_, err := askTool.Execute(context.Background(), args)
		if err == nil || err.Error() != "question is required" {
			t.Errorf("args=%v: expected 'question is required', got %v", args, err)
		}
	}
}

func TestAskExecutePassesQuestionToElicitation(t *testing.T) {
	var got string
	elicit := &tool.Elicitation{
		Ask: func(_ context.Context, q string) (string, error) {
			got = q
			return "user-reply", nil
		},
	}
	askTool := Tools(elicit)[0]

	result, err := askTool.Execute(context.Background(), map[string]any{
		"question": "Which database?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Which database?" {
		t.Errorf("Ask received %q, want %q", got, "Which database?")
	}
	if result != "user-reply" {
		t.Errorf("result = %q, want user-reply", result)
	}
}

func TestAskExecutePropagatesElicitationError(t *testing.T) {
	sentinel := errors.New("user declined")
	elicit := &tool.Elicitation{
		Ask: func(context.Context, string) (string, error) { return "", sentinel },
	}
	askTool := Tools(elicit)[0]

	_, err := askTool.Execute(context.Background(), map[string]any{
		"question": "?",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error to propagate, got: %v", err)
	}
}
