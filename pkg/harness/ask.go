package harness

import (
	"context"
	"strings"
)

const sideQuestion = "Side question from the user, apart from the task above. Answer it directly and concisely from what this conversation already shows. Do not call tools and do not continue the task: the answer is shown to the user on its own and is not added to this conversation. If the conversation does not hold the answer, say so.\n\nQuestion: "

const sideBriefing = "The transcript is a coding session in progress. Answer the user's side question about it directly and concisely, from the transcript alone. If it does not hold the answer, say so.\n\nQuestion: "

const sideAnswerTokens = 4096

// Ask answers a side question from the session's current context. The request
// repeats the cached prefix with tool selection disabled and records only its
// usage, so it may run while a turn is in progress. onText receives the answer
// as it arrives; the returned text is the answer to keep.
func (a *Agent) Ask(ctx context.Context, question string, onText func(string)) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", ErrEmptyInput
	}

	messages := a.contextSnapshot()
	if update, changed := a.contextInstructionsUpdate(messages); changed {
		messages = append(messages, update)
	}
	// A turn in progress may hold calls that still run.
	messages, _ = closeToolCalls(messages, "This tool call has not returned yet.")

	req := &request{cacheKey: a.CacheKey, tools: a.AvailableTools()}
	if a.Model != nil {
		req.model = a.Model()
	}
	if a.Effort != nil {
		req.effort = a.Effort()
	}
	if a.Instructions != nil {
		req.instructions = a.Instructions()
	}

	resp, err := a.completeInContext(ctx, req, messages, sideQuestion+question, func(message Message, _ error) bool {
		for _, content := range message.Content {
			if content.Text != "" && onText != nil {
				onText(content.Text)
			}
		}
		return true
	})
	if err == nil {
		if answer := visibleAssistantText(resp.messages); answer != "" {
			return answer, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if isTerminalProviderError(err) {
		return "", err
	}
	// The bounded briefing gets past a history the provider would not replay.
	return a.generateBriefing(ctx, sideBriefing+question, messages, sideAnswerTokens)
}
