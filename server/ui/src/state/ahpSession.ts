import { ChatInputAnswerState, ChatInputAnswerValueKind } from "./ahpValues.ts";
import type {
	ChatState,
	SessionState,
	RootState,
	ResponsePart,
	Turn,
	ActiveTurn,
	Message,
	ChatInputAnswer,
	ChatInputQuestion,
	ConfigPropertySchema,
	SessionConfigPropertySchema,
	ToolCallState,
	ToolResultContent,
} from "@microsoft/agent-host-protocol";
import type {
	ChatEntry,
	PendingPrompt,
	PendingTurnInput,
} from "../types/session.ts";
import type { Phase, PromptField, ToolLocation } from "../types/protocol.ts";
import type { ModelInfo } from "../api/models.ts";
import type { ModeOption } from "../api/sessions.ts";
import {
	emptySession,
	type SessionFields,
	type SessionSettings,
	type SessionView,
} from "./sessionStore.ts";

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
function strings(property?: ConfigPropertySchema): string[] {
	return (property?.enum ?? []).map(String);
}
function options(property?: ConfigPropertySchema) {
	return strings(property).map((id, index) => ({
		id,
		name: property?.enumLabels?.[index] || id,
		description: property?.enumDescriptions?.[index],
	}));
}

/** Maps a file URI with an optional `#L<line>` fragment into the workspace. */
export function toolLocation(
	uri: string,
	directory: string,
): ToolLocation | undefined {
	let url: URL;
	try {
		url = new URL(uri);
	} catch {
		return;
	}
	const line = /^#L(\d+)/.exec(url.hash)?.[1];
	url.hash = "";
	let path =
		url.protocol === "file:" ? decodeURIComponent(url.pathname) : url.href;
	if (directory.startsWith("file:")) {
		const root = decodeURIComponent(new URL(directory).pathname).replace(
			/\/$/,
			"",
		);
		if (path.startsWith(`${root}/`)) path = path.slice(root.length + 1);
	}
	return { path, ...(line ? { line: Number(line) } : {}) };
}

function toolKind(tool: ToolCallState): string | undefined {
	const kind = tool._meta?.toolKind;
	if (typeof kind !== "string") return;
	return kind === "terminal" ? "execute" : kind;
}

export function chatSettings(
	session: SessionState,
	chat: ChatState,
	root: RootState,
): SessionSettings {
	const properties =
		chat.config?.schema?.properties ?? session.config?.schema.properties ?? {};
	// Session values mirror the default chat; a chat's own values win.
	const values = { ...session.config?.values, ...chat.config?.values };
	return configSettings(session.provider, properties, values, root);
}

/** Settings from a config schema and values, described by the root catalogue. */
export function configSettings(
	provider: string,
	properties: Record<string, SessionConfigPropertySchema>,
	values: Record<string, unknown>,
	root: RootState,
): SessionSettings {
	const value = (name: string) => values[name] ?? properties[name]?.default;
	const agent = root.agents.find((a) => a.provider === provider);
	const catalogue = agent?.models ?? [];
	// The schema lists the selectable models; the root catalogue describes them.
	const describe = (id: string, label?: string): ModelInfo => {
		const model = catalogue.find((m) => m.id === id);
		return {
			id,
			name: label || model?.name || id,
			namespace: model?.vendor,
			efforts: strings(model?.configSchema?.properties.effort),
		};
	};
	const models = properties.model?.enum?.length
		? strings(properties.model).map((id, index) =>
				describe(id, properties.model.enumLabels?.[index]),
			)
		: catalogue.map((model) => describe(model.id, model.name));
	const model = String(value("model") ?? "");
	const selected = models.find((m) => m.id === model);
	const modes: ModeOption[] = options(properties.mode);
	return {
		models,
		model,
		effort: String(value("effort") ?? ""),
		efforts: selected?.efforts?.length
			? selected.efforts
			: strings(properties.effort),
		modes,
		mode: String(value("mode") ?? ""),
		...(properties.unattended
			? { unattended: value("unattended") === true }
			: {}),
		canFork: agent?.capabilities?.multipleChats?.fork === true,
	};
}

export function chatPhase(chat: ChatState): Phase {
	if (!chat.activeTurn) return "idle";
	const last = chat.activeTurn.responseParts.at(-1);
	if (last?.kind === "markdown") return "streaming";
	if (last?.kind === "toolCall") return "tool_running";
	return "thinking";
}

/** The context meter follows the latest request; totals sum every turn. */
export function chatUsage(
	chat: ChatState,
	root: RootState,
	provider: string,
	model: string,
): SessionFields["usage"] {
	const usage = { ...emptySession("").usage };
	let latest: Turn["usage"];
	for (const turn of chat.activeTurn
		? [...chat.turns, chat.activeTurn]
		: chat.turns) {
		if (!turn.usage) continue;
		latest = turn.usage;
		usage.inputTokens += turn.usage.inputTokens ?? 0;
		usage.outputTokens += turn.usage.outputTokens ?? 0;
		usage.cachedTokens += turn.usage.cacheReadTokens ?? 0;
	}
	usage.lastInputTokens = latest?.inputTokens ?? 0;
	const models = root.agents.find((a) => a.provider === provider)?.models;
	usage.contextWindow =
		models?.find((m) => m.id === (latest?.model || model))?.maxContextWindow ??
		0;
	return usage;
}

function chatError(session: SessionState, chat: ChatState): string | null {
	if (session.creationError) return session.creationError.message;
	const last = chat.activeTurn ? undefined : chat.turns.at(-1);
	if (last?.state === "cancelled") return "Cancelled";
	if (last?.state !== "error") return null;
	for (const part of last.responseParts)
		if (part.kind === "error") return part.error.message;
	return "The turn failed";
}

function questionField(question: ChatInputQuestion): PromptField {
	const field: PromptField = {
		name: question.id,
		title: question.title,
		description: question.message,
		required: question.required,
		type:
			question.kind === "boolean" ||
			question.kind === "number" ||
			question.kind === "integer"
				? question.kind
				: "string",
	};
	if ("options" in question) {
		const recommended = question.options
			.filter((option) => option.recommended)
			.map((option) => option.id);
		return {
			...field,
			enum: question.options.map((option) => option.id),
			enum_labels: question.options.map((option) => option.label),
			enum_descriptions: question.options.map(
				(option) => option.description ?? "",
			),
			strict: !question.allowFreeformInput,
			multiple: question.kind === "multi-select",
			...(recommended.length
				? {
						default:
							question.kind === "multi-select" ? recommended : recommended[0],
					}
				: {}),
		};
	}
	return question.defaultValue !== undefined
		? { ...field, default: question.defaultValue }
		: field;
}

/** Cache by the SDK reducers' immutable object identities: a token only
 * rebuilds its active turn; completed turns and unchanged tool entries retain
 * their React identities. */
export class AHPSessionProjection {
	private turns = new WeakMap<Turn | ActiveTurn, ChatEntry[]>();
	private parts = new WeakMap<ResponsePart, ChatEntry | null>();
	private completed?: Turn[];
	private completedEntries: ChatEntry[] = [];
	// Settings and usage keep their identities across token updates.
	private memo = new Map<string, { key: unknown[]; value: unknown }>();
	private cached<T>(slot: string, key: unknown[], compute: () => T): T {
		const previous = this.memo.get(slot);
		if (previous?.key.every((value, i) => value === key[i]))
			return previous.value as T;
		const value = compute();
		this.memo.set(slot, { key, value });
		return value;
	}
	private readonly directory: string;
	/** `directory` is the workspace's file URI that tool locations are relative to. */
	constructor(directory = "") {
		this.directory = directory;
	}
	private result(content: ToolResultContent[] = []) {
		const text: string[] = [],
			images: string[] = [],
			locations: ToolLocation[] = [];
		for (const item of content) {
			if (item.type === "text") text.push(item.text);
			else if (
				item.type === "embeddedResource" &&
				item.contentType.startsWith("image/")
			)
				images.push(`data:${item.contentType};base64,${item.data}`);
			else if (item.type === "resource") {
				const location = toolLocation(item.uri, this.directory);
				if (location) locations.push(location);
			}
		}
		return { text: text.filter(Boolean).join("\n"), images, locations };
	}
	private part(part: ResponsePart, id: string): ChatEntry | null {
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
				...(part.kind === "reasoning" && part.duration !== undefined
					? { reasoningDuration: part.duration }
					: {}),
			};
		} else if (part.kind === "systemNotification") {
			const content = display(part.content);
			if (content) entry = { id, type: "notice", content };
		} else if (
			part.kind === "toolCall" &&
			!part.toolCall.toolCallId.startsWith("approval:")
		) {
			const tool = part.toolCall;
			const result =
				tool.status === "completed" || tool.status === "running"
					? this.result(tool.content)
					: undefined;
			entry = {
				id: tool.toolCallId,
				toolId: tool.toolCallId,
				type: "tool",
				content: "",
				toolName: tool.toolName,
				toolKind: toolKind(tool),
				toolHint: tool.intention,
				toolArgs:
					tool.status === "streaming"
						? (tool.partialInput ?? "")
						: typeof tool.toolInput === "string"
							? tool.toolInput
							: "",
				toolPartial: tool.status === "streaming",
				...(result?.locations.length
					? { toolLocations: result.locations }
					: {}),
				...(result?.images.length ? { images: result.images } : {}),
				toolResult:
					tool.status === "completed"
						? result!.text || (tool.error?.message ?? "")
						: tool.status === "cancelled"
							? display(tool.reasonMessage)
							: undefined,
			};
		}
		this.parts.set(part, entry);
		return entry;
	}
	private turn(turn: Turn | ActiveTurn): ChatEntry[] {
		let result = this.turns.get(turn);
		if (!result) {
			result = [
				{
					id: `user:${turn.id}`,
					inputId: turn.id,
					type: "user",
					content: turn.message.text,
					...attachments(turn.message),
				},
			];
			turn.responseParts.forEach((part, index) => {
				const entry = this.part(part, `${turn.id}:${index}`);
				if (entry) result!.push(entry);
			});
			this.turns.set(turn, result);
		}
		return result;
	}
	project(
		key: string,
		session: SessionState,
		chat: ChatState,
		revision: number,
		{
			uri = "",
			root = { agents: [] },
		}: { uri?: string; root?: RootState } = {},
	): SessionView {
		if (this.completed !== chat.turns) {
			this.completed = chat.turns;
			this.completedEntries = chat.turns.flatMap((turn) => this.turn(turn));
		}
		const settings = this.cached(
			"settings",
			[session.config, session.provider, chat.config, root.agents],
			() => chatSettings(session, chat, root),
		);
		const prompts: PendingPrompt[] = [];
		for (const part of chat.activeTurn?.responseParts ?? []) {
			if (
				part.kind === "toolCall" &&
				part.toolCall.status === "pending-confirmation"
			) {
				prompts.push({
					id: part.toolCall.toolCallId,
					kind: "confirm",
					message: display(part.toolCall.invocationMessage),
					options: part.toolCall.options?.map(({ id, label, kind, group }) => ({
						id,
						label,
						kind,
						...(group !== undefined ? { group } : {}),
					})),
				});
			} else if (part.kind === "inputRequest" && !part.response) {
				prompts.push({
					id: part.request.id,
					kind: "ask",
					message: part.request.message ?? "",
					fields: (part.request.questions ?? []).map(questionField),
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
		const error = chatError(session, chat);
		return {
			...emptySession(key),
			id: key,
			session: uri,
			provider: session.provider,
			status: session.creationError ? "error" : "ready",
			phase: chatPhase(chat),
			...(chat.activity ? { activity: chat.activity } : {}),
			error,
			usage: this.cached(
				"usage",
				[chat.turns, chat.activeTurn?.usage, root.agents, settings.model],
				() => chatUsage(chat, root, session.provider, settings.model),
			),
			settings,
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
			case "integer":
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
