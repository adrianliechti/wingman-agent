package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const finishToolName = "finish_turn"

var ErrMissingFinish = fmt.Errorf("%w: the model did not confirm completion after two reminders. Send Continue to resume", ErrTurnIncomplete)

const finishInstructions = `End your turn by calling finish_turn with your final user-facing answer in its answer field. The harness displays that answer to the user. If you already wrote the answer as ordinary text after the latest tool results or user input, call finish_turn with {} without repeating it. Call finish_turn without other tool calls. Use it when the requested work is complete or your answer asks for information or authorization needed to proceed. Progress updates do not finish a turn: continue with the work and tools they describe.`

const finishReminder = `Your turn is still active. If work remains, continue it using tools. If your last message was already your final answer or a question for the user, call finish_turn with {} alone and do not repeat the answer.`

const missingAnswerReminder = `You have not sent a user-facing answer since the latest tool work or user input. If work remains, continue it. Otherwise, call finish_turn with your final answer in its answer field. Tool results and thinking are not a user-facing answer. Do not repeat completed tool work.`

func finishTool() tool.Tool {
	return tool.Tool{
		Name:        finishToolName,
		Description: "Ends your turn and displays answer to the user. Include your final answer here, or use {} if it was already sent after the latest tool results. Call without other tools.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"answer": map[string]any{"type": "string", "description": "Final user-facing answer. Omit if already sent as text after the latest tool results or user input."},
		}, "additionalProperties": false},
		Hidden: true,
	}
}

// The finish marker is a harness protocol operation, not an executable tool.
// Record a result for replay, including rejected markers, but never dispatch it
// through user tools or let it skip a batch of actual work. previousAnswer is
// the completed text after the latest tool work or user input, allowing a bare
// marker and avoiding duplicate answers without discarding a different one.
func (a *Agent) resolveFinishCalls(resp *response, calls []ToolCall, previousAnswer string, yield func(Message, error) bool) ([]ToolCall, bool, error) {
	visibleAnswer := strings.TrimSpace(lastAssistantText(resp.messages))
	if visibleAnswer == "" {
		visibleAnswer = strings.TrimSpace(previousAnswer)
	}
	var work []ToolCall
	finished := false
	for _, call := range calls {
		if call.Name != finishToolName {
			work = append(work, call)
			continue
		}
		var args map[string]any
		valid := json.Unmarshal([]byte(call.Args), &args) == nil && args != nil
		var answer string
		for key, value := range args {
			text, ok := value.(string)
			if key != "answer" || !ok || strings.TrimSpace(text) == "" {
				valid = false
			}
			if key == "answer" {
				answer = text
			}
		}
		var rejection string
		switch {
		case !valid:
			rejection = `Use {"answer":"your final answer"}, or {} if the answer was already sent.`
		case len(calls) != 1:
			rejection = "Other tool calls are still pending. Wait for their results, write your final user-facing answer, then call finish_turn without other tool calls."
		case resp.incomplete:
			rejection = "The response was cut off. Complete your user-facing answer before calling finish_turn."
		case resp.stopReason == "pause_turn":
			rejection = "The provider paused the response. Continue the unfinished work before calling finish_turn."
		case visibleAnswer == "" && answer == "":
			rejection = "You have not written a user-facing answer since the latest tool work or user input. Call finish_turn with your final answer (or a question needed to proceed) in its answer field. Do not repeat completed tool work."
		}
		result := tool.Error("Finish rejected. " + rejection)
		if rejection == "" {
			finished = true
			result = tool.Text("Turn finished.")
		}
		message := toolResultMessage(call, result)
		message.Hidden = true
		messages := []Message{message}
		var final Message
		if finished && answer != "" && strings.TrimSpace(answer) != visibleAnswer {
			final = Message{Role: RoleAssistant, Phase: PhaseFinalAnswer, Content: []Content{{Text: answer, TextID: "finish_answer_" + call.ID}}}
			messages = append(messages, final)
		}
		if err := a.appendMessages(messages...); err != nil {
			return nil, false, err
		}
		if final.Role != "" {
			// Persist the validated answer with its marker result before showing
			// it. Include it in this response for answer tracking and Stop hooks.
			resp.messages = append(resp.messages, final)
			if !yield(final, nil) {
				return nil, false, errYieldStopped
			}
		}
	}
	return work, finished, nil
}

func hasRefusal(messages []Message) bool {
	for _, message := range messages {
		for _, content := range message.Content {
			if content.Refusal != "" {
				return true
			}
		}
	}
	return false
}
