package harness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

const finishToolName = "finish_turn"

// Captured with the tools for each request, including across transport retries.
type finishMode uint8

const (
	finishDisabled finishMode = iota
	finishAutomatic
	finishConfirming
)

func (r *request) configureFinish(confirming bool) error {
	for _, t := range r.tools {
		if t.Name == finishToolName {
			return fmt.Errorf("agent: tool name %q is reserved for turn confirmation", finishToolName)
		}
	}
	r.finishMode = finishAutomatic
	if confirming {
		r.finishMode = finishConfirming
		r.tools = append(slices.Clone(r.tools), finishTool())
		if r.instructions != "" {
			r.instructions += "\n\n"
		}
		r.instructions += finishInstructions
	}
	return nil
}

type finishStatus uint8

const (
	finishAbsent finishStatus = iota
	finishAccepted
	finishRejected
)

var ErrMissingFinish = fmt.Errorf("%w: the model did not confirm completion after two reminders. Send Continue to resume", ErrTurnIncomplete)

const finishInstructions = `End your turn by calling finish_turn with your final user-facing answer in its answer field. The harness displays that answer to the user. Only omit the answer if you already sent it in an explicitly marked final_answer message after the latest tool results or user input. Call finish_turn without other tool calls. Use it when the requested work is complete or your answer asks for information or authorization needed to proceed. Progress updates do not finish a turn: continue with the work and tools they describe.`

// Earlier versions persisted these tool-specific reminders in session history.
const finishReminder = `Your turn is still active. If work remains, continue it using tools. If your last message was already your final answer or a question for the user, call finish_turn with {} alone and do not repeat the answer.`

const missingAnswerReminder = `Your turn is still active. Continue any outstanding work using tools. Once finished, call finish_turn with your final answer or a question needed to proceed in its answer field. Do not repeat completed tool work.`

const legacyMissingAnswerReminder = `You have not sent a user-facing answer since the latest tool work or user input. If work remains, continue it. Otherwise, call finish_turn with your final answer in its answer field. Tool results and thinking are not a user-facing answer. Do not repeat completed tool work.`

func finishTool() tool.Tool {
	return tool.Tool{
		Name:        finishToolName,
		Description: "Ends your turn and displays answer to the user. Include your final answer here, or use {} after an explicit final_answer message. Call without other tools.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"answer": map[string]any{"type": "string", "description": "Final user-facing answer. Omit only after an explicit final_answer message since the latest tool results or user input."},
		}, "additionalProperties": false},
		Hidden: true,
	}
}

func (r *request) handlesFinishTool(name string) bool {
	return r.finishMode != finishDisabled && name == finishToolName
}

func isLegacyFinishReminder(m Message) bool {
	if m.Role != RoleUser || !m.Hidden || len(m.Content) != 1 {
		return false
	}
	text := m.Content[0].Text
	return text == finishReminder || text == missingAnswerReminder || text == legacyMissingAnswerReminder
}

// The finish marker is a harness protocol operation, not an executable tool.
// Record a result for replay, including rejected markers, but never dispatch it
// through user tools or let it skip a batch of actual work.
func (a *Agent) resolveFinishCalls(req *request, resp *response, calls []ToolCall, previous assistantText, yield func(Message, error) bool) ([]ToolCall, finishStatus, error) {
	if !slices.ContainsFunc(calls, func(c ToolCall) bool { return req.handlesFinishTool(c.Name) }) {
		return calls, finishAbsent, nil
	}

	visible := lastAssistantTextItem(resp.messages)
	if visible.isProgress() {
		visible = assistantText{}
	} else if strings.TrimSpace(visible.text) == "" {
		visible = previous
	}
	var work []ToolCall
	status := finishRejected
	for _, call := range calls {
		if call.Name != finishToolName {
			work = append(work, call)
			continue
		}
		answer, valid := finishAnswer(call.Args)
		var rejection string
		switch {
		case !valid:
			rejection = `Use {"answer":"your final answer"}, or {} after an explicit final_answer message.`
		case len(calls) != 1:
			rejection = "Other tool calls are still pending. Wait for their results, write your final user-facing answer, then call finish_turn without other tool calls."
		case resp.incomplete:
			rejection = "The response was cut off. Complete your user-facing answer before calling finish_turn."
		case resp.stopReason == "pause_turn":
			rejection = "The provider paused the response. Continue the unfinished work before calling finish_turn."
		case answer == "" && (visible.phase != PhaseFinalAnswer || strings.TrimSpace(visible.text) == ""):
			rejection = "A bare finish requires an explicit final_answer message since the latest tool work or user input. Call finish_turn with your final answer (or a question needed to proceed) in its answer field. Do not repeat completed tool work."
		}
		// A model can mimic a tool from history after a switch. Only an
		// advertised finish tool can override a native request to continue.
		if req.finishMode != finishConfirming && (rejection != "" || resp.endTurn != nil && !*resp.endTurn) {
			rejection = "finish_turn is unavailable for this request. Write your final user-facing answer directly as assistant text after completing any remaining tool work."
		}
		result := tool.Error("Finish rejected. " + rejection)
		if rejection == "" {
			status = finishAccepted
			result = tool.Text("Turn finished.")
		}
		message := toolResultMessage(call, result)
		message.Hidden = true
		messages := []Message{message}
		var final Message
		if status == finishAccepted && answer != "" && strings.TrimSpace(answer) != strings.TrimSpace(visible.text) {
			final = Message{Role: RoleAssistant, Phase: PhaseFinalAnswer, Content: []Content{{Text: answer, TextID: "finish_answer_" + call.ID}}}
			messages = append(messages, final)
		}
		if err := a.appendMessages(messages...); err != nil {
			return nil, finishRejected, err
		}
		if final.Role != "" {
			// Persist the validated answer with its marker result before showing
			// it. Include it in this response for answer tracking and Stop hooks.
			resp.messages = append(resp.messages, final)
			if !yield(final, nil) {
				return nil, finishRejected, errYieldStopped
			}
		}
	}
	return work, status, nil
}

// A bare object acknowledges an earlier answer. Otherwise the entire payload
// must be one nonblank answer string, whose contents are returned verbatim.
func finishAnswer(arguments string) (string, bool) {
	var args map[string]string
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return "", false
	}
	if len(args) == 0 {
		return "", true
	}
	answer := args["answer"]
	return answer, len(args) == 1 && strings.TrimSpace(answer) != ""
}
