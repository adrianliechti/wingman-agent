package agent

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const finishToolName = "finish_turn"
const maxFinishReminders = 2

var ErrMissingFinish = errors.New("agent: model did not explicitly finish its turn after two reminders")

const finishInstructions = `End your turn by writing your final user-facing answer, then calling finish_turn with {} in the same response. Call finish_turn alone, without other tool calls. Use it when the requested work is complete or your answer asks for information or authorization needed to proceed. Progress updates do not finish a turn: continue with the work and tools they describe.`

const finishReminder = `Your turn is still active. Continue the requested work using tools. If the work is complete or you need input from the user, write your final answer and call finish_turn with {} in the same response, without other tool calls.`

// The protocol rules live in the system prompt; the description only needs to
// identify the tool so the model does not read two versions of the same rule.
func finishTool() tool.Tool {
	return tool.Tool{
		Name:        finishToolName,
		Description: "Ends your turn. Call it alone with {} in the same response as your final user-facing answer.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Hidden:      true,
	}
}

// The finish marker is a harness protocol operation, not an executable tool.
// Record a result for replay, including rejected markers, but never dispatch it
// through user tools or let it skip a batch of actual work.
func (a *Agent) resolveFinishCalls(resp *response, calls []ToolCall) ([]ToolCall, bool, error) {
	var work []ToolCall
	finished := false
	for _, call := range calls {
		if call.Name != finishToolName {
			work = append(work, call)
			continue
		}
		var args map[string]any
		valid := json.Unmarshal([]byte(call.Args), &args) == nil && args != nil && len(args) == 0
		accepted := valid && len(calls) == 1 && !resp.incomplete && strings.TrimSpace(lastAssistantText(resp.messages)) != ""
		result := tool.Error("Finish rejected. " + finishReminder)
		if accepted {
			finished = true
			result = tool.Text("Turn finished.")
		}
		message := toolResultMessage(call, result)
		message.Hidden = true
		if err := a.appendMessages(message); err != nil {
			return nil, false, err
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
