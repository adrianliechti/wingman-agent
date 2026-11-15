import type { MessageAttachment } from "@microsoft/agent-host-protocol";
import type {
	PromptKind,
	PromptField,
	PromptOption,
	ToolLocation,
	TurnInputState,
	TurnInputIntent,
} from "./protocol.ts";
export interface PendingPrompt {
	id: string;
	kind: PromptKind;
	message: string;
	fields?: PromptField[];
	options?: PromptOption[];
}

export interface ChatEntry {
	id: string;
	type: "user" | "assistant" | "tool" | "reasoning" | "notice";
	content: string;
	images?: string[];
	files?: string[];
	/** The message's attachments that are neither files nor images, as sent. */
	attachments?: MessageAttachment[];
	inputId?: string;
	toolName?: string;
	toolKind?: string;
	toolArgs?: string;
	toolLocations?: ToolLocation[];
	toolHint?: string;
	toolResult?: string;
	toolId?: string;
	toolPartial?: boolean;
	reasoningId?: string;
	reasoningDuration?: number;
}

export interface PendingTurnInput {
	id: string;
	state: TurnInputState;
	origin?: string;
	intent: TurnInputIntent;
	position: number;
	text: string;
	files: string[];
	images: string[];
	attachments?: MessageAttachment[];
	error?: string;
}
