import type { Page, WebSocketRoute } from "@playwright/test";
import { emptySession, sessionKey } from "../src/state/sessionStore.ts";
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
	settings: ReturnType<typeof emptySession>["settings"];
	prompts: PendingPrompt[];
	pendingInputs: PendingTurnInput[];
	canSteer: boolean;
};
const fixtures = new WeakMap<Page, AHPFixture>();
const chatURI = (uri: string) =>
	uri.replace("ahp-session:", "ahp-chat:") + "/chat";
const fixtureURI = (backend: string, id: string) =>
	`ahp-session:/fixture/${encodeURIComponent(backend)}/${encodeURIComponent(id)}`;

/** Browser presentation fixtures speak AHP. Real integration tests exercise the
 * Go host; these fixtures deliberately control latency and transcript contents. */
export class AHPFixture {
	sessions = new Map<string, FixtureSession>();
	backends = new Set<string>();
	connections = new Set<WebSocketRoute>();
	subscriptions = new Map<string, Set<WebSocketRoute>>();
	offline = false;
	sequence = 0;
	onLoad = (_session: FixtureSession) => {};
	onCreate?: (params: any) => Promise<FixtureSession>;
	onCommand?: (command: any, session: FixtureSession) => Promise<void>;
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
			canSteer: false,
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
			_meta: {
				"wingman.ref": {
					workspaceId: this.scope.workspaceId,
					backendId: session.backend,
					sessionId: session.id,
				},
			},
		};
	}
	state(session: FixtureSession) {
		const ref = {
			workspaceId: this.scope.workspaceId,
			backendId: session.backend,
			sessionId: session.id,
		};
		return {
			...this.summary(session),
			lifecycle: "ready",
			chats: [],
			activeClients: [],
			config: { schema: { type: "object", properties: {} }, values: {} },
			_meta: {
				wingman: {
					...emptySession(sessionKey(session.backend, session.id)),
					ref,
					epoch: `fixture:${session.id}`,
					status: "ready",
					phase: session.phase,
					settings: session.settings,
					canSteer: session.canSteer,
				},
			},
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
						_meta: { "wingman.entry": user },
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
			else if (entry.type === "tool")
				parts.push({
					kind: "toolCall",
					toolCall: {
						toolCallId: entry.toolId ?? entry.id,
						toolName: entry.toolName ?? "Tool",
						displayName: entry.toolName ?? "Tool",
						status: entry.toolResult !== undefined ? "completed" : "running",
						toolInput: entry.toolArgs ?? "",
						invocationMessage: entry.toolName ?? "Tool",
						confirmed: "not-needed",
						success: true,
						pastTenseMessage: entry.toolName ?? "Tool",
						content:
							entry.toolResult === undefined
								? undefined
								: [{ type: "text", text: entry.toolResult }],
						_meta: { wingman: entry },
					},
				});
		}
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
						},
					});
				else
					activeTurn.responseParts.push({
						kind: "inputRequest",
						request: {
							id: prompt.id,
							message: prompt.message,
							questions: (prompt.fields ?? []).map((field) => ({
								id: field.name,
								title: field.title,
								message: field.description ?? field.name,
								required: field.required,
								kind: field.enum?.length
									? field.multiple
										? "multi-select"
										: "single-select"
									: field.type === "boolean" || field.type === "number"
										? field.type
										: "text",
								options: field.enum?.map((id) => ({ id, label: id })),
								allowFreeformInput: !field.strict,
							})),
						},
					});
			}
		return {
			resource: chatURI(session.resource),
			title: session.title,
			status: activeTurn ? 2 : 1,
			modifiedAt: session.updated_at,
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
		this.send(session.resource, {
			type: "session/metaChanged",
			_meta: this.state(session)._meta,
		});
		const channel = chatURI(session.resource),
			chat = this.chat(session);
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
					if (
						request.method === "x-wingman/resolveSession" &&
						this.backends.has(p.backend)
					) {
						const session = [...this.sessions.values()].find(
							(s) => s.backend === p.backend && s.id === p.session,
						);
						if (!session) {
							server.send(wire);
							return;
						}
						reply({ resource: session.resource });
						return;
					}
					const session = this.find(p.channel);
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
							action.type === "session/configChanged"
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
										}
									: action.type === "chat/toolCallConfirmed"
										? {
												type: "prompt_response",
												action: action.approved ? "accept" : "decline",
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
		const scope = await (await page.request.get("/api/v2/bootstrap")).json();
		fixture = new AHPFixture(scope);
		fixtures.set(page, fixture);
		await fixture.install(page);
	}
	return fixture;
}
