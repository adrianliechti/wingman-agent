import { ChatInputAnswerState, ChatInputAnswerValueKind } from "./ahpValues.ts";
import type {
	ChatState,
	SessionState,
	ResponsePart,
	Turn,
	ActiveTurn,
	Message,
	ChatInputAnswer,
} from "@microsoft/agent-host-protocol";
import type {
	ChatEntry,
	PendingPrompt,
	PendingTurnInput,
} from "../types/session.ts";
import type { PromptField } from "../types/protocol.ts";
import {
	emptySession,
	type SessionFields,
	type SessionRef,
	type SessionView,
} from "./sessionStore.ts";

export type RuntimeInfo = Omit<SessionFields, "prompts" | "pendingInputs"> & {
	ref: SessionRef;
	epoch: string;
};
export function runtimeInfo(session: SessionState): RuntimeInfo {
	const info = session._meta?.wingman as RuntimeInfo | undefined;
	if (!info?.ref || !info.epoch)
		throw new Error("Host does not provide Wingman session metadata");
	return info;
}
function attachments(message: Message) {
	const files: string[] = [],
		images: string[] = [];
	for (const a of message.attachments ?? []) {
		if (a.type === "resource") files.push(a.label);
		else if (
			a.type === "embeddedResource" &&
			a.contentType.startsWith("image/")
		)
			images.push(`data:${a.contentType};base64,${a.data}`);
	}
	return { files, images };
}
function display(value: string | { markdown: string } | undefined): string {
	return typeof value === "string" ? value : (value?.markdown ?? "");
}

/** Cache by the SDK reducers' immutable object identities: a token only
 * rebuilds its active turn; completed turns and unchanged tool entries retain
 * their React identities. */
export class AHPSessionProjection {
	private turns = new WeakMap<Turn | ActiveTurn, ChatEntry[]>();
	private parts = new WeakMap<ResponsePart, ChatEntry | null>();
	private completed?: Turn[];
	private completedEntries: ChatEntry[] = [];
	private part(part: ResponsePart): ChatEntry | null {
		if (this.parts.has(part)) return this.parts.get(part)!;
		let entry: ChatEntry | null = null;
		if (part.kind === "markdown" || part.kind === "reasoning") {
			const images: string[] = [];
			const content =
				part.kind === "markdown"
					? part.content.replace(
							/\n\n!\[Image\]\((data:image\/[a-zA-Z0-9.+-]+;base64,[a-zA-Z0-9+/=]+)\)/g,
							(_match, image: string) => {
								images.push(image);
								return "";
							},
						)
					: part.content;
			entry = {
				id: part.id,
				type: part.kind === "markdown" ? "assistant" : "reasoning",
				content,
				...(images.length ? { images } : {}),
			};
		} else if (part.kind === "systemNotification") {
			const input = part._meta?.["wingman.input"] as ChatEntry | undefined;
			if (
				input?.type === "user" &&
				typeof input.id === "string" &&
				typeof input.content === "string"
			)
				entry = input;
		} else if (
			part.kind === "toolCall" &&
			!part.toolCall.toolCallId.startsWith("approval:")
		) {
			const tool = part.toolCall;
			const extra = tool._meta?.wingman as Partial<ChatEntry> | undefined;
			entry = {
				...extra,
				id: extra?.id ?? tool.toolCallId,
				toolId: tool.toolCallId,
				type: "tool",
				content: "",
				toolName: tool.toolName,
				toolArgs:
					tool.status === "streaming"
						? (tool.partialInput ?? "")
						: "toolInput" in tool
							? typeof tool.toolInput === "string"
								? tool.toolInput
								: ""
							: "",
				toolPartial: tool.status === "streaming",
				toolResult:
					tool.status === "completed"
						? (tool.content ?? [])
								.map((c) => (c.type === "text" ? c.text : ""))
								.filter(Boolean)
								.join("\n")
						: undefined,
			};
		}
		this.parts.set(part, entry);
		return entry;
	}
	private turn(turn: Turn | ActiveTurn): ChatEntry[] {
		let result = this.turns.get(turn);
		if (!result) {
			const identity = turn.message._meta?.["wingman.entry"] as
				| { id?: string; messageId?: string }
				| undefined;
			result = [
				{
					id: identity?.id ?? `user:${turn.id}`,
					messageId: identity?.messageId,
					inputId: turn.id,
					type: "user",
					content: turn.message.text,
					...attachments(turn.message),
				},
			];
			for (const part of turn.responseParts) {
				const entry = this.part(part);
				if (entry) result.push(entry);
			}
			this.turns.set(turn, result);
		}
		return result;
	}
	project(
		key: string,
		session: SessionState,
		chat: ChatState,
		revision: number,
	): SessionView {
		if (this.completed !== chat.turns) {
			this.completed = chat.turns;
			this.completedEntries = chat.turns.flatMap((turn) => this.turn(turn));
		}
		const info = runtimeInfo(session);
		const prompts: PendingPrompt[] = [];
		for (const part of chat.activeTurn?.responseParts ?? []) {
			if (
				part.kind === "toolCall" &&
				part.toolCall.status === "pending-confirmation"
			) {
				prompts.push({
					id: part.toolCall.toolCallId.replace(/^approval:/, ""),
					kind: "confirm",
					message: display(part.toolCall.invocationMessage),
				});
			} else if (part.kind === "inputRequest" && !part.response) {
				const fields: PromptField[] = (part.request.questions ?? []).map(
					(q) => ({
						name: q.id,
						title: q.title,
						description: q.message,
						required: q.required,
						type:
							q.kind === "boolean"
								? "boolean"
								: q.kind === "number"
									? "number"
									: "string",
						...("options" in q
							? {
									enum: q.options.map((o) => o.id),
									enum_descriptions: q.options.map(
										(o) => o.description ?? o.label,
									),
									strict: !q.allowFreeformInput,
									multiple: q.kind === "multi-select",
								}
							: {}),
					}),
				);
				prompts.push({
					id: part.request.id,
					kind: "ask",
					message: part.request.message ?? "",
					fields,
				});
			}
		}
		const pendingInputs: PendingTurnInput[] = (chat.queuedMessages ?? []).map(
			(pending, position) => ({
				id: pending.id,
				text: pending.message.text,
				...attachments(pending.message),
				state: "queued",
				intent: pending.kind === "steering" ? "steer" : "follow_up",
				position,
				origin: "user",
			}),
		);
		if (
			chat.steeringMessage &&
			!pendingInputs.some((input) => input.id === chat.steeringMessage!.id)
		)
			pendingInputs.push({
				id: chat.steeringMessage.id,
				text: chat.steeringMessage.message.text,
				...attachments(chat.steeringMessage.message),
				state: "queued",
				intent: "steer",
				position: 0,
				origin: "user",
			});
		pendingInputs.sort(
			(a, b) => Number(b.intent === "steer") - Number(a.intent === "steer"),
		);
		return {
			...emptySession(key),
			...info,
			id: key,
			entries: chat.activeTurn
				? [...this.completedEntries, ...this.turn(chat.activeTurn)]
				: this.completedEntries,
			prompts,
			pendingInputs,
			revision,
			synchronized: true,
		};
	}
}

export function inputAnswers(
	chat: ChatState,
	requestId: string,
	content: Record<string, unknown>,
): Record<string, ChatInputAnswer> {
	const part = chat.activeTurn?.responseParts.find(
		(p) => p.kind === "inputRequest" && p.request.id === requestId,
	);
	const answers: Record<string, ChatInputAnswer> = Object.create(null);
	if (part?.kind !== "inputRequest") return answers;
	for (const question of part.request.questions ?? []) {
		const value = content[question.id];
		if (value === undefined) continue;
		switch (question.kind) {
			case "boolean":
				answers[question.id] = {
					state: ChatInputAnswerState.Submitted,
					value: {
						kind: ChatInputAnswerValueKind.Boolean,
						value: Boolean(value),
					},
				};
				break;
			case "number":
				answers[question.id] = {
					state: ChatInputAnswerState.Submitted,
					value: {
						kind: ChatInputAnswerValueKind.Number,
						value: Number(value),
					},
				};
				break;
			case "single-select": {
				const text = String(value),
					selected = question.options.some((option) => option.id === text);
				answers[question.id] = {
					state: ChatInputAnswerState.Submitted,
					value: {
						kind: ChatInputAnswerValueKind.Selected,
						value: selected ? text : "",
						...(!selected ? { freeformValues: [text] } : {}),
					},
				};
				break;
			}
			case "multi-select": {
				const values = Array.isArray(value)
					? value.map(String)
					: [String(value)];
				const selected = values.filter((value) =>
					question.options.some((option) => option.id === value),
				);
				answers[question.id] = {
					state: ChatInputAnswerState.Submitted,
					value: {
						kind: ChatInputAnswerValueKind.SelectedMany,
						value: selected,
						freeformValues: values.filter((value) => !selected.includes(value)),
					},
				};
				break;
			}
			default:
				answers[question.id] = {
					state: ChatInputAnswerState.Submitted,
					value: { kind: ChatInputAnswerValueKind.Text, value: String(value) },
				};
		}
	}
	return answers;
}
