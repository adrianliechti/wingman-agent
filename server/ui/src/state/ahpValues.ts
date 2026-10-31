import type * as Protocol from "@microsoft/agent-host-protocol";

// The official SDK declares const enums, which cannot be imported as values under
// TypeScript's verbatimModuleSyntax. Keep the few client-written discriminants
// typed against the SDK without changing the application's compiler settings.
export const ActionType = {
	ChatPendingMessageSet:
		"chat/pendingMessageSet" as Protocol.ActionType.ChatPendingMessageSet,
	ChatPendingMessageRemoved:
		"chat/pendingMessageRemoved" as Protocol.ActionType.ChatPendingMessageRemoved,
	ChatTurnCancelled:
		"chat/turnCancelled" as Protocol.ActionType.ChatTurnCancelled,
	ChatToolCallConfirmed:
		"chat/toolCallConfirmed" as Protocol.ActionType.ChatToolCallConfirmed,
	ChatInputCompleted:
		"chat/inputCompleted" as Protocol.ActionType.ChatInputCompleted,
	ChatConfigChanged:
		"chat/configChanged" as Protocol.ActionType.ChatConfigChanged,
	TerminalInput: "terminal/input" as Protocol.ActionType.TerminalInput,
	TerminalResized: "terminal/resized" as Protocol.ActionType.TerminalResized,
	TerminalClaimed: "terminal/claimed" as Protocol.ActionType.TerminalClaimed,
};
export const ToolCallConfirmationReason = {
	UserAction: "user-action" as Protocol.ToolCallConfirmationReason.UserAction,
};
export const ToolCallCancellationReason = {
	Denied: "denied" as Protocol.ToolCallCancellationReason.Denied,
};
export const ChatSourceKind = {
	Fork: "fork" as Protocol.ChatSourceKind.Fork,
};
export const PendingMessageKind = {
	Queued: "queued" as Protocol.PendingMessageKind.Queued,
	Steering: "steering" as Protocol.PendingMessageKind.Steering,
};
export const ChatInputAnswerState = {
	Submitted: "submitted" as Protocol.ChatInputAnswerState.Submitted,
};
export const MessageAttachmentKind = {
	Resource: "resource" as Protocol.MessageAttachmentKind.Resource,
	EmbeddedResource:
		"embeddedResource" as Protocol.MessageAttachmentKind.EmbeddedResource,
};
export const ChatInputAnswerValueKind = {
	Boolean: "boolean" as Protocol.ChatInputAnswerValueKind.Boolean,
	Number: "number" as Protocol.ChatInputAnswerValueKind.Number,
	Text: "text" as Protocol.ChatInputAnswerValueKind.Text,
	Selected: "selected" as Protocol.ChatInputAnswerValueKind.Selected,
	SelectedMany:
		"selected-many" as Protocol.ChatInputAnswerValueKind.SelectedMany,
};
