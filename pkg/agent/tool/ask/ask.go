package ask

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func Tools(elicit *tool.Elicitation) []tool.Tool {
	if elicit == nil || elicit.Ask == nil {
		return nil
	}

	description := strings.Join([]string{
		"Ask the user a question and wait for their reply. Blocks until they answer.",
		"- Make a reasonable assumption first; ask only when the answer cannot be discovered from local context and being wrong would force a meaningful redo.",
		"- Ask at most one question per turn. Keep it concrete and actionable; if there are 2-3 viable alternatives, list the tradeoff for each in the question.",
		"- Do not ask for permission to proceed with an obvious next step, for preferences that do not materially affect the result, or for information you can get by reading/searching the workspace.",
		"- Not for tool-execution yes/no confirmations (handled by the harness).",
	}, "\n")

	return []tool.Tool{{
		Name:        "ask_user",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectReadOnly),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "Question for the user."},
			},

			"required": []string{"question"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			question, ok := args["question"].(string)

			if !ok || question == "" {
				return "", fmt.Errorf("question is required")
			}

			return elicit.Ask(ctx, question)
		},

		Hidden: true,
	}}
}
