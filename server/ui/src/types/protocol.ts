// Client → Server. `session` routes the message to one of the server's
// in-memory sessions; concurrent sessions stream independently.

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

export type ClientMessage = SendMessage | CancelMessage;

// Server → Client. Per-session events carry `session`; workspace-level
// events leave it unset. The hook routes per-session events into the
// sessions map; subscribers see every frame (used by panels that refetch
// on workspace-level signals).

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

export type ServerMessage =
	| SessionStateMessage
	| TextDeltaMessage
	| ReasoningDeltaMessage
	| ToolCallMessage
	| ToolResultMessage
	| PhaseMessage
	| UsageMessage
	| ErrorMessage
	| CheckpointsChangedMessage
	| SessionsChangedMessage
	| DiffsChangedMessage
	| FilesChangedMessage
	| DiagnosticsChangedMessage
	| CapabilitiesChangedMessage;

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
