import type { Page, WebSocketRoute } from "@playwright/test";
import {
	canonicalChatURI,
	chatToSession,
	emptySession,
	sessionToChat,
} from "../src/state/sessionStore.ts";
import type {
	ChatEntry,
	PendingPrompt,
	PendingTurnInput,
} from "../src/types/session.ts";

type Saved = { id: string; title: string; updated_at: string };
export type FixtureSession = Saved & {
	backend: string;
	resource: string;
	entries: ChatEntry[];
	phase: string;
	activity?: string;
	settings: ReturnType<typeof emptySession>["settings"];
	prompts: PendingPrompt[];
	pendingInputs: PendingTurnInput[];
};
const fixtures = new WeakMap<Page, AHPFixture>();
const chatURI = sessionToChat;
/** Saved fixture sessions use the host's canonical URIs, so legacy links resolve. */
export const fixtureChat = canonicalChatURI;
const fixtureURI = (backend: string, id: string) =>
	chatToSession(canonicalChatURI(backend, id));
const approvalOptions = [
	{ id: "once", label: "Allow", kind: "approve" },
	{ id: "session", label: "Allow for this session", kind: "approve", group: 1 },
	{ id: "deny", label: "Deny", kind: "deny" },
];
const choices = (
	items: { id: string; name: string; description?: string }[],
) => ({
	enum: items.map((item) => item.id),
	enumLabels: items.map((item) => item.name),
	enumDescriptions: items.map((item) => item.description ?? ""),
});

/** Browser presentation fixtures speak AHP. Real integration tests exercise the
 * Go host; these fixtures deliberately control latency and transcript contents. */
export class AHPFixture {
	resourceHandlers: {
		method: string;
		path?: string;
		run: (route: any) => Promise<void>;
	}[] = [];
	resourceObservers = new Set<(request: any, reply?: any) => void>();
	sessions = new Map<string, FixtureSession>();
	backends = new Set<string>();
	/** Display names of the providers the fixture adds to the host's root. */
	agents = new Map<string, string>();
	connections = new Set<WebSocketRoute>();
	subscriptions = new Map<string, Set<WebSocketRoute>>();
	offline = false;
	sequence = 0;
	directory = "";
	onLoad = (_session: FixtureSession) => {};
	onCreate?: (params: any) => Promise<FixtureSession>;
	onCommand?: (command: any, session: FixtureSession) => Promise<void>;
	onCompletions?: (params: any, session: FixtureSession) => any[];
	constructor(readonly scope: { workspaceId: string; instanceId: string }) {}
	add(
		backend: string,
		saved: Saved,
		entries: ChatEntry[] = [],
		phase = "idle",
	) {
		this.backends.add(backend);
		const session: FixtureSession = {
			...saved,
			backend,
			entries,
			phase,
			resource: fixtureURI(backend, saved.id),
			settings: emptySession("").settings,
			prompts: [],
			pendingInputs: [],
		};
		this.sessions.set(session.resource, session);
		return session;
	}
	find(channel: string) {
		return [...this.sessions.values()].find(
			(s) => s.resource === channel || chatURI(s.resource) === channel,
		);
	}
	summary(session: FixtureSession) {
		return {
			resource: session.resource,
			provider: session.backend,
			title: session.title,
			status: 1,
			createdAt: session.updated_at,
			modifiedAt: session.updated_at,
			defaultChat: chatURI(session.resource),
			chats: [{ resource: chatURI(session.resource), title: session.title }],
		};
	}
	values(session: FixtureSession) {
		const { model, mode, effort, unattended } = session.settings;
		return {
			model,
			mode,
			effort,
			...(unattended !== undefined ? { unattended } : {}),
		};
	}
	state(session: FixtureSession) {
		const { settings } = session;
		const properties: Record<string, unknown> = {};
		if (settings.models.length)
			properties.model = {
				type: "string",
				title: "Model",
				chatMutable: true,
				...choices(settings.models),
			};
		if (settings.modes.length)
			properties.mode = {
				type: "string",
				title: "Mode",
				chatMutable: true,
				...choices(settings.modes),
			};
		if (settings.efforts.length)
			properties.effort = {
				type: "string",
				title: "Effort",
				chatMutable: true,
				enum: settings.efforts,
			};
		if (settings.unattended !== undefined)
			properties.unattended = {
				type: "boolean",
				title: "Unattended",
				chatMutable: true,
			};
		return {
			provider: session.backend,
			title: session.title,
			status: 1,
			lifecycle: "ready",
			activeClients: [],
			chats: [
				{
					resource: chatURI(session.resource),
					title: session.title,
					status: 1,
					modifiedAt: session.updated_at,
				},
			],
			defaultChat: chatURI(session.resource),
			config: {
				schema: { type: "object", properties },
				values: this.values(session),
			},
		};
	}
	tool(entry: ChatEntry) {
		const kind = entry.toolKind === "execute" ? "terminal" : entry.toolKind;
		const locations = (entry.toolLocations ?? []).map((location) => ({
			type: "resource",
			uri:
				new URL(
					location.path.split("/").map(encodeURIComponent).join("/"),
					this.directory.replace(/\/$/, "") + "/",
				).href + (location.line ? `#L${location.line}` : ""),
		}));
		return {
			toolCallId: entry.toolId ?? entry.id,
			toolName: entry.toolName ?? "Tool",
			displayName: entry.toolName ?? "Tool",
			...(entry.toolHint ? { intention: entry.toolHint } : {}),
			status: entry.toolPartial
				? "streaming"
				: entry.toolResult !== undefined
					? "completed"
					: "running",
			...(entry.toolPartial
				? { partialInput: entry.toolArgs ?? "" }
				: { toolInput: entry.toolArgs ?? "" }),
			invocationMessage: entry.toolName ?? "Tool",
			confirmed: "not-needed",
			success: true,
			pastTenseMessage: entry.toolName ?? "Tool",
			content: [
				...(entry.toolResult === undefined
					? []
					: [{ type: "text", text: entry.toolResult }]),
				...locations,
			],
			...(kind ? { _meta: { toolKind: kind } } : {}),
		};
	}
	question(field: NonNullable<PendingPrompt["fields"]>[number]) {
		const select = !!field.enum?.length;
		const defaults = Array.isArray(field.default)
			? field.default.map(String)
			: field.default != null
				? [String(field.default)]
				: [];
		return {
			id: field.name,
			title: field.title,
			message: field.description ?? field.name,
			required: field.required,
			kind: select
				? field.multiple
					? "multi-select"
					: "single-select"
				: field.type === "boolean" ||
					  field.type === "number" ||
					  field.type === "integer"
					? field.type
					: "text",
			...(select
				? {
						options: field.enum!.map((id, index) => ({
							id,
							label: field.enum_labels?.[index] ?? id,
							...(field.enum_descriptions?.[index]
								? { description: field.enum_descriptions[index] }
								: {}),
							...(defaults.includes(id) ? { recommended: true } : {}),
						})),
						allowFreeformInput: !field.strict,
					}
				: field.default !== undefined
					? { defaultValue: field.default }
					: {}),
		};
	}
	chat(session: FixtureSession): any {
		const turns: any[] = [];
		for (const entry of session.entries) {
			if (entry.type === "user" || !turns.length) {
				const user =
					entry.type === "user"
						? entry
						: { id: "fixture-user", content: "Fixture" };
				const attachments = ("images" in user ? (user.images ?? []) : []).map(
					(image) => {
						const [prefix, data] = image.split(",");
						return {
							type: "embeddedResource",
							label: "Image",
							contentType: prefix.slice(5).replace(";base64", ""),
							data,
						};
					},
				);
				turns.push({
					id: "inputId" in user ? (user.inputId ?? user.id) : user.id,
					startedAt: session.updated_at,
					duration: 0,
					state: "complete",
					message: {
						text: user.content,
						origin: { kind: "user" },
						attachments,
					},
					responseParts: [],
				});
				if (entry.type === "user") continue;
			}
			const parts = turns.at(-1).responseParts;
			if (entry.type === "assistant" || entry.type === "reasoning")
				parts.push({
					kind: entry.type === "assistant" ? "markdown" : "reasoning",
					id: entry.id,
					content:
						entry.content +
						(entry.images ?? []).map((src) => `\n\n![Image](${src})`).join(""),
				});
			else if (entry.type === "notice")
				parts.push({ kind: "systemNotification", content: entry.content });
			else if (entry.type === "tool")
				parts.push({ kind: "toolCall", toolCall: this.tool(entry) });
		}
		// Clients derive the phase from the active turn, so a busy session needs one.
		if (session.phase !== "idle" && !turns.length)
			turns.push({
				id: "fixture-turn",
				startedAt: session.updated_at,
				state: "complete",
				message: { text: "Fixture", origin: { kind: "user" } },
				responseParts:
					session.phase === "tool_running"
						? [
								{
									kind: "toolCall",
									toolCall: {
										toolCallId: "fixture-tool",
										toolName: "fixture",
										displayName: "Fixture",
										status: "running",
										invocationMessage: "Fixture",
										confirmed: "not-needed",
									},
								},
							]
						: [],
			});
		const activeTurn = session.phase !== "idle" ? turns.pop() : undefined;
		if (activeTurn)
			for (const prompt of session.prompts) {
				if (prompt.kind === "confirm")
					activeTurn.responseParts.push({
						kind: "toolCall",
						toolCall: {
							toolCallId: `approval:${prompt.id}`,
							toolName: "Approval",
							displayName: "Approval",
							status: "pending-confirmation",
							invocationMessage: prompt.message,
							toolInput: "",
							options: prompt.options ?? approvalOptions,
						},
					});
				else
					activeTurn.responseParts.push({
						kind: "inputRequest",
						request: {
							id: prompt.id,
							message: prompt.message,
							questions: (prompt.fields ?? []).map((field) =>
								this.question(field),
							),
						},
					});
			}
		return {
			resource: chatURI(session.resource),
			title: session.title,
			status: activeTurn ? 2 : 1,
			modifiedAt: session.updated_at,
			...(session.activity ? { activity: session.activity } : {}),
			turns,
			activeTurn,
			queuedMessages: session.pendingInputs.map((input) => ({
				id: input.id,
				kind: input.intent === "steer" ? "steering" : "queued",
				message: { text: input.text, origin: { kind: "user" } },
			})),
		};
	}
	send(channel: string, action: any) {
		const params = { channel, action, serverSeq: ++this.sequence };
		for (const socket of this.subscriptions.get(channel) ?? [])
			socket.send(JSON.stringify({ jsonrpc: "2.0", method: "action", params }));
	}
	publish(session: FixtureSession) {
		const values = this.values(session);
		this.send(session.resource, {
			type: "session/configChanged",
			config: values,
		});
		const channel = chatURI(session.resource),
			chat = this.chat(session);
		this.send(channel, { type: "chat/configChanged", config: values });
		this.send(channel, {
			type: "chat/activityChanged",
			activity: chat.activity,
		});
		this.send(channel, { type: "chat/truncated" });
		this.send(channel, { type: "chat/turnsLoaded", turns: chat.turns });
		if (chat.activeTurn) {
			const { id, startedAt, message, responseParts } = chat.activeTurn;
			this.send(channel, {
				type: "chat/turnStarted",
				turnId: id,
				startedAt,
				message,
			});
			for (const part of responseParts)
				this.send(channel, { type: "chat/responsePart", turnId: id, part });
		}
	}
	private withAgents(agents: any[]) {
		const known = new Set(agents.map((agent) => agent.provider));
		const extra = [...new Set([...this.backends, ...this.agents.keys()])]
			.filter((id) => !known.has(id))
			.map((id) => ({
				provider: id,
				displayName: this.agents.get(id) ?? id,
				description: "",
				models: [],
			}));
		return [...agents, ...extra];
	}
	disconnect() {
		this.offline = true;
		for (const socket of this.connections) socket.close();
	}
	async install(page: Page) {
		await page.routeWebSocket(/\/ahp(?:\?|$)/, (socket) => {
			if (this.offline) {
				socket.close();
				return;
			}
			this.connections.add(socket);
			const server = socket.connectToServer();
			const waiting = new Map<unknown, (reply: any) => void>();
			const forwarded = new Map<unknown, any>();
			server.onMessage((wire) => {
				const message = JSON.parse(String(wire));
				if (message.result?.defaultDirectory)
					this.directory = message.result.defaultDirectory;
				for (const snapshot of message.result?.snapshots ?? [])
					if (snapshot.resource === "ahp-root://")
						snapshot.state.agents = this.withAgents(snapshot.state.agents);
				if (message.params?.action?.type === "root/agentsChanged")
					message.params.action.agents = this.withAgents(
						message.params.action.agents,
					);
				const request = forwarded.get(message.id);
				if (request) {
					for (const observer of this.resourceObservers)
						observer(request, message);
					forwarded.delete(message.id);
				}
				const resolve = waiting.get(message.id);
				if (resolve) {
					waiting.delete(message.id);
					resolve(message);
				} else socket.send(JSON.stringify(message));
			});
			let clientId = "";
			socket.onClose(() => {
				this.connections.delete(socket);
				for (const peers of this.subscriptions.values()) peers.delete(socket);
				server.close();
			});
			socket.onMessage(async (wire) => {
				const request = JSON.parse(String(wire));
				const p = request.params ?? {};
				const reply = (result: any) => {
					if (request.id !== undefined)
						socket.send(
							JSON.stringify({ jsonrpc: "2.0", id: request.id, result }),
						);
				};
				const reject = (message: string) =>
					socket.send(
						JSON.stringify({
							jsonrpc: "2.0",
							id: request.id,
							error: { code: -32603, message },
						}),
					);
				try {
					if (request.method === "initialize") clientId = p.clientId;
					for (const observer of this.resourceObservers) observer(request);
					const resourceHandler = [...this.resourceHandlers]
						.reverse()
						.find(
							(handler) =>
								handler.method === request.method &&
								(!handler.path ||
									decodeURIComponent(p.uri ?? "").endsWith("/" + handler.path)),
						);
					if (resourceHandler) {
						await resourceHandler.run({
							params: p,
							fetch: async () => {
								const response = await new Promise<any>((resolve) => {
									waiting.set(request.id, resolve);
									forwarded.set(request.id, request);
									server.send(wire);
								});
								return { json: async () => response.result, frame: response };
							},
							fulfill: async ({ response, json }: any) => {
								socket.send(
									JSON.stringify(
										response?.frame && json === undefined
											? response.frame
											: { jsonrpc: "2.0", id: request.id, result: json },
									),
								);
							},
							continue: async () => {
								forwarded.set(request.id, request);
								server.send(wire);
							},
						});
						return;
					}
					forwarded.set(request.id, request);

					if (
						request.method === "resolveSessionConfig" &&
						this.backends.has(p.provider)
					) {
						reply({ schema: { type: "object", properties: {} }, values: {} });
						return;
					}
					if (request.method === "listSessions") {
						reply({
							items: [...this.sessions.values()].map((s) => this.summary(s)),
						});
						return;
					}
					if (
						request.method === "createSession" &&
						this.backends.has(p.provider) &&
						this.onCreate
					) {
						if (!this.sessions.has(p.channel)) {
							const session = await this.onCreate(p);
							this.sessions.delete(session.resource);
							session.resource = p.channel;
							this.sessions.set(p.channel, session);
						}
						reply({});
						return;
					}
					const session = this.find(p.channel);
					if (
						session &&
						(request.method === "disposeSession" ||
							request.method === "disposeChat")
					) {
						this.sessions.delete(session.resource);
						for (const peer of this.connections)
							peer.send(
								JSON.stringify({
									jsonrpc: "2.0",
									method: "root/sessionRemoved",
									params: { session: session.resource },
								}),
							);
						reply({});
						return;
					}
					if (session && request.method === "completions") {
						reply({ items: this.onCompletions?.(p, session) ?? [] });
						return;
					}
					if (session && request.method === "subscribe") {
						this.onLoad(session);
						const peers = this.subscriptions.get(p.channel) ?? new Set();
						peers.add(socket);
						this.subscriptions.set(p.channel, peers);
						reply({
							snapshot: {
								resource: p.channel,
								fromSeq: this.sequence,
								state:
									p.channel === session.resource
										? this.state(session)
										: this.chat(session),
							},
						});
						return;
					}
					if (session && request.method === "unsubscribe") {
						this.subscriptions.get(p.channel)?.delete(socket);
						reply({});
						return;
					}
					if (session && request.method === "dispatchAction") {
						const action = p.action;
						const command =
							action.type === "chat/configChanged"
								? { type: "settings", ...action.config }
								: action.type === "chat/pendingMessageSet"
									? {
											type: session.pendingInputs.some(
												(input) => input.id === action.id,
											)
												? "queue_update"
												: "send",
											id: action.id,
											inputId: action.id,
											text: action.message.text,
											attachments: action.message.attachments,
										}
									: action.type === "chat/toolCallConfirmed"
										? {
												type: "prompt_response",
												action: action.approved ? "accept" : "decline",
												option: action.selectedOptionId,
												promptId: action.toolCallId.replace("approval:", ""),
											}
										: action.type === "chat/inputCompleted"
											? {
													type: "prompt_response",
													action: action.response,
													promptId: action.requestId,
													content: Object.fromEntries(
														Object.entries(action.answers ?? {}).map(
															([key, answer]: [string, any]) => [
																key,
																answer.value?.kind === "selected-many"
																	? [
																			...answer.value.value,
																			...(answer.value.freeformValues ?? []),
																		]
																	: (answer.value?.freeformValues?.[0] ??
																		answer.value?.value),
															],
														),
													),
												}
											: { type: action.type };
						try {
							await this.onCommand?.(command, session);
							if (command.type === "settings") {
								Object.assign(session.settings, action.config);
								this.publish(session);
							}
							socket.send(
								JSON.stringify({
									jsonrpc: "2.0",
									method: "action",
									params: {
										...p,
										serverSeq: ++this.sequence,
										origin: { clientId, clientSeq: p.clientSeq },
									},
								}),
							);
							if (command.type === "send")
								this.send(p.channel, {
									type: "chat/pendingMessageRemoved",
									kind: action.kind,
									id: action.id,
								});
						} catch (error) {
							socket.send(
								JSON.stringify({
									jsonrpc: "2.0",
									method: "action",
									params: {
										...p,
										serverSeq: ++this.sequence,
										origin: { clientId, clientSeq: p.clientSeq },
										rejectionReason: String(error),
									},
								}),
							);
						}
						return;
					}
					server.send(wire);
				} catch (error) {
					reject(String(error));
				}
			});
		});
	}
}

export async function ahpFixture(page: Page) {
	let fixture = fixtures.get(page);
	if (!fixture) {
		const scope = await (await page.request.get("/api/workspace")).json();
		fixture = new AHPFixture(scope);
		fixtures.set(page, fixture);
		await fixture.install(page);
	}
	return fixture;
}

export async function mockResource(
	page: Page,
	method: string,
	run: (route: any) => Promise<void>,
	path?: string,
) {
	const fixture = await ahpFixture(page);
	fixture.resourceHandlers.push({ method, path, run });
}
export async function waitResource(
	page: Page,
	method: string,
	path?: string,
	result = false,
) {
	const fixture = await ahpFixture(page);
	return new Promise<void>((resolve) => {
		const listener = (request: any, reply?: any) => {
			if (
				request.method === method &&
				(!path ||
					decodeURIComponent(request.params?.uri ?? "").endsWith("/" + path)) &&
				(!result || reply)
			) {
				fixture.resourceObservers.delete(listener);
				resolve();
			}
		};
		fixture.resourceObservers.add(listener);
	});
}
