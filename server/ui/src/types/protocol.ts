interface SendMessage {
	type: "send";
	session: string;
	text: string;
	files?: string[];
	images?: string[];
}

interface CancelMessage {
	type: "cancel";
	session: string;
}

interface PromptResponseMessage {
	type: "prompt_response";
	session: string;
	prompt_id: string;
	text?: string;
	approved?: boolean;
}

interface FocusMessage {
	type: "focus";
}

export type ClientMessage =
	| SendMessage
	| CancelMessage
	| PromptResponseMessage
	| FocusMessage;

interface SessionStateMessage {
	type: "session_state";
	session: string;
	phase: Phase;
	messages?: ConversationMessage[];
	input_tokens?: number;
	cached_tokens?: number;
	output_tokens?: number;
}

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
}

interface UsageMessage {
	type: "usage";
	session: string;
	input_tokens: number;
	cached_tokens: number;
	output_tokens: number;
}

interface ErrorMessage {
	type: "error";
	session: string;
	message: string;
}

export type PromptKind = "ask" | "confirm";

interface PromptMessage {
	type: "prompt";
	session: string;
	prompt_id: string;
	prompt_kind: PromptKind;
	message: string;
}

interface PromptCancelMessage {
	type: "prompt_cancel";
	session: string;
	prompt_id: string;
}

interface CheckpointsChangedMessage {
	type: "checkpoints_changed";
	session?: string;
}

interface SessionsChangedMessage {
	type: "sessions_changed";
}

interface DiffsChangedMessage {
	type: "diffs_changed";
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

interface AgentChangedMessage {
	type: "agent_changed";
}

interface ModelChangedMessage {
	type: "model_changed";
}

export type ServerMessage =
	| SessionStateMessage
	| TextDeltaMessage
	| ReasoningDeltaMessage
	| ToolCallMessage
	| ToolResultMessage
	| PhaseMessage
	| UsageMessage
	| ErrorMessage
	| PromptMessage
	| PromptCancelMessage
	| CheckpointsChangedMessage
	| SessionsChangedMessage
	| DiffsChangedMessage
	| FilesChangedMessage
	| DiagnosticsChangedMessage
	| CapabilitiesChangedMessage
	| AgentChangedMessage
	| ModelChangedMessage;

export type Phase = "idle" | "thinking" | "streaming" | "tool_running";

export interface ConversationMessage {
	role: string;
	content: ConversationContent[];
}

interface ConversationContent {
	text?: string;
	image?: {
		data: string;
		name?: string;
	};
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
		id?: string;
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
	content?: string;
	language?: string;
	binary?: boolean;
	mime?: string;
	size: number;
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
