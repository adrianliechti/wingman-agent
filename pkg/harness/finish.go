package harness

import "slices"

// Earlier versions persisted these tool-specific reminders in session history.
const finishReminder = `Your turn is still active. If work remains, continue it using tools. If your last message was already your final answer or a question for the user, call finish_turn with {} alone and do not repeat the answer.`

const missingAnswerReminder = `Your turn is still active. Continue any outstanding work using tools. Once finished, call finish_turn with your final answer or a question needed to proceed in its answer field. Do not repeat completed tool work.`

const legacyMissingAnswerReminder = `You have not sent a user-facing answer since the latest tool work or user input. If work remains, continue it. Otherwise, call finish_turn with your final answer in its answer field. Tool results and thinking are not a user-facing answer. Do not repeat completed tool work.`

const confirmationReminder = `<system-reminder>Your last message did not end your turn. If work remains, continue it using tools. If that message already was your final answer, or a question the user must answer before you can proceed, do not start new work, repeat checks, or write the answer again: end the turn the way your instructions describe.</system-reminder>`

// Saved marker calls and results were separate hidden messages. Their answers
// were persisted as ordinary assistant text and stay in the conversation.
func migrateFinishHistory(messages []Message) ([]Message, bool) {
	replaced := false
	kept := slices.DeleteFunc(messages, func(m Message) bool {
		if !m.Hidden || len(m.Content) != 1 {
			return false
		}
		c := &m.Content[0]
		switch m.Role {
		case RoleAssistant:
			return c.ToolCall != nil && c.ToolCall.Name == "finish_turn" ||
				c.ToolResult != nil && c.ToolResult.Name == "finish_turn"
		case RoleUser:
			switch c.Text {
			case finishReminder, confirmationReminder:
				return true
			case missingAnswerReminder, legacyMissingAnswerReminder:
				c.Text = continuationReminder
				replaced = true
			}
		}
		return false
	})
	return kept, replaced || len(kept) != len(messages)
}
