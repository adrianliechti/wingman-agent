// Client -> Server
interface SendMessage {
	type: "send";
	session: string;
	text: string;
	files?: string[];
}

interface CancelMessage {
	type: "cancel";
	session: string;
}

interface PromptResponseMessage {
	type: "prompt_response";
	session: string;
	approved: boolean;
}

interface AskResponseMessage {
	type: "ask_response";
	session: string;
	answer: string;
}

export type ClientMessage =
	| SendMessage
	| CancelMessage
	| PromptResponseMessage
	| AskResponseMessage;

// Server -> Client. `session` is set on every event that pertains to a
// specific conversation; events about workspace-level state (files, diffs,
// sessions list, capabilities) leave it unset and apply to every view.
interface TextDeltaMessage {
	type: "text_delta";
	session: string;
	text: string;
}

interface ReasoningDeltaMessage {
	type: "reasoning_delta";
	session: string;
	id: string;
	text: string;
}

interface ToolCallMessage {
	type: "tool_call";
	session: string;
	id: string;
	name: string;
	args: string;
	hint: string;
}

interface ToolResultMessage {
	type: "tool_result";
	session: string;
	id: string;
	name: string;
	content: string;
}

interface PhaseMessage {
	type: "phase";
	session: string;
	phase: Phase;
	hint?: string;
}

interface PromptMessage {
	type: "prompt";
	session: string;
	question: string;
}

interface AskMessage {
	type: "ask";
	session: string;
	question: string;
}

interface ErrorMessage {
	type: "error";
	session: string;
	message: string;
}

interface DoneMessage {
	type: "done";
	session: string;
}

interface UsageMessage {
	type: "usage";
	session: string;
	input_tokens: number;
	cached_tokens: number;
	output_tokens: number;
}

interface MessagesMessage {
	type: "messages";
	session: string;
	messages: ConversationMessage[];
}

interface SessionMessage {
	type: "session";
	session: string;
	id: string;
}

interface DiffsChangedMessage {
	type: "diffs_changed";
	session?: string;
}

interface CheckpointsChangedMessage {
	type: "checkpoints_changed";
	session?: string;
}

interface SessionsChangedMessage {
	type: "sessions_changed";
}

interface FilesChangedMessage {
	type: "files_changed";
}

interface DiagnosticsChangedMessage {
	type: "diagnostics_changed";
}

interface CapabilitiesChangedMessage {
	type: "capabilities_changed";
}

export type ServerMessage =
	| TextDeltaMessage
	| ReasoningDeltaMessage
	| ToolCallMessage
	| ToolResultMessage
	| PhaseMessage
	| PromptMessage
	| AskMessage
	| ErrorMessage
	| DoneMessage
	| UsageMessage
	| MessagesMessage
	| SessionMessage
	| DiffsChangedMessage
	| CheckpointsChangedMessage
	| SessionsChangedMessage
	| FilesChangedMessage
	| DiagnosticsChangedMessage
	| CapabilitiesChangedMessage;

// Shared types
export type Phase = "idle" | "thinking" | "streaming" | "tool_running";

export interface ConversationMessage {
	role: string;
	content: ConversationContent[];
}

interface ConversationContent {
	text?: string;
	reasoning?: {
		id?: string;
		summary?: string;
	};
	tool_call?: {
		id: string;
		name: string;
		args: string;
		hint?: string;
	};
	tool_result?: {
		name: string;
		args: string;
		content: string;
	};
}

export interface FileEntry {
	name: string;
	path: string;
	is_dir: boolean;
	size: number;
}

export interface FileContent {
	path: string;
	content: string;
	language: string;
}

export interface DiffEntry {
	path: string;
	status: "added" | "modified" | "deleted";
	patch: string;
	original?: string;
	modified?: string;
	language?: string;
}

export interface CheckpointEntry {
	hash: string;
	message: string;
	time: string;
}

export interface DiagnosticEntry {
	path: string;
	line: number;
	column: number;
	severity: "error" | "warning" | "info";
	message: string;
	source?: string;
}
