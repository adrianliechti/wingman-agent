import type { WorkspaceInfo } from "../api/workspace.ts";
import { ChangesetClient, workspaceChangeset } from "./changesetClient.ts";
import { FileClient } from "./fileClient.ts";
import { DAPClient } from "./dapClient.ts";
import { LSPClient } from "./lspClient.ts";
import {
	MessageKind,
	type ChatState,
	type SessionState,
	type SessionSummary,
	type Message,
	type MessageAttachment,
	type ChatInputResponseKind,
} from "@microsoft/agent-host-protocol";
import {
	ActionType,
	ChatSourceKind,
	PendingMessageKind,
	MessageAttachmentKind,
	ToolCallCancellationReason,
	ToolCallConfirmationReason,
} from "./ahpValues.ts";
import { RpcError } from "@microsoft/agent-host-protocol/client";
import { AHPConnection } from "./ahpConnection.ts";
import { AHPSessionProjection, inputAnswers } from "./ahpSession.ts";
import {
	APIError,
	createWorkspaceHTTP,
	setWorkspaceHTTP,
} from "../api/http.ts";
import type { PromptReply, ServerMessage } from "../types/protocol.ts";
import type { SessionInfo } from "../api/sessions.ts";
import {
	SessionStore,
	canonicalBackend,
	chatToSession,
	draftBackend,
	emptySession,
	isDraft,
	sessionToChat,
	type SettingsPatch,
	type WorkspaceScope,
} from "./sessionStore.ts";

export type Bootstrap = WorkspaceScope & { protocol: number };
export async function fetchWorkspace(): Promise<WorkspaceInfo & Bootstrap> {
	const response = await fetch("/api/workspace");
	if (!response.ok) throw new APIError(await response.text(), response.status);
	const scope = (await response.json()) as WorkspaceInfo;
	if (scope.protocol !== 2 || !scope.instanceId || !scope.workspaceId)
		throw new Error("Unsupported workspace protocol. Reload the application.");
	return scope;
}

type SendRequest = {
	id: string;
	text: string;
	files: string[];
	images: string[];
	intent: string;
	completions: MessageAttachment[];
};
export function chatPath(key: string): string {
	return `/api/v2/chats/${encodeURIComponent(key)}`;
}
/** The URI a canonical subscription failed over to, when the host aliases it. */
function aliasOf(error: unknown): string | undefined {
	if (!(error instanceof RpcError) || error.code !== -32602) return;
	const resource = (error.data as { resource?: unknown } | undefined)?.resource;
	return typeof resource === "string" ? resource : undefined;
}

export class WorkspaceClient {
	readonly store = new SessionStore();
	readonly scope: Bootstrap;
	readonly http: ReturnType<typeof createWorkspaceHTTP>;
	readonly ahp: AHPConnection;
	readonly lsp: LSPClient;
	readonly dap: DAPClient;
	readonly files: FileClient;
	readonly changesets: ChangesetClient;
	private observed = new Map<string, { count: number; release: () => void }>();
	/** Chat URI to the URI of the session that owns it. */
	private sessions = new Map<string, string>();
	private providers = new Map<string, string>();
	private catalogue?: Promise<void>;
	private redirects = new Map<string, string>();
	private redirectListeners = new Set<(from: string, to: string) => void>();
	private sends = new Map<
		string,
		{ request: SendRequest; pending?: Promise<void> }
	>();
	private forks = new Map<string, { uri: string; pending?: Promise<string> }>();
	private creations = new Map<
		string,
		{ uri: string; pending?: Promise<string> }
	>();
	constructor(scope: Bootstrap, ahp = new AHPConnection(scope)) {
		this.lsp = new LSPClient(scope.instanceId);
		this.dap = new DAPClient(scope.instanceId);
		this.files = new FileClient(ahp, this.lsp);
		this.changesets = new ChangesetClient(ahp);
		this.scope = scope;
		this.ahp = ahp;
		this.http = createWorkspaceHTTP(scope.instanceId);
		ahp.subscribeConnection(() => {
			if (!ahp.getConnected()) this.store.disconnect();
		});
	}
	readonly getReplaced = () => this.ahp.getReplaced();
	/** Connects now instead of waiting for the next retry. */
	reconnect() {
		void this.ahp.ready().catch(() => {});
	}
	readonly getConnected = () => this.ahp.getConnected();
	readonly subscribeConnection = (listener: () => void) =>
		this.ahp.subscribeConnection(listener);
	readonly subscribe = (listener: (event: ServerMessage) => void) => {
		const releaseHost = this.ahp.subscribe(listener);
		const releaseDiagnostics = this.lsp.connection.onEvent((method) => {
			if (method === "workspace/diagnostic/refresh")
				listener({ type: "diagnostics_changed" });
		});
		return () => {
			releaseHost();
			releaseDiagnostics();
		};
	};
	/** A migrated chat key lives under another URI; owners replace the key. */
	readonly onRedirect = (listener: (from: string, to: string) => void) => {
		this.redirectListeners.add(listener);
		return () => {
			this.redirectListeners.delete(listener);
		};
	};
	start() {
		this.ahp.start();
		// Changeset actions are the client's only signal that workspace changes moved.
		this.changesets.watch(workspaceChangeset());
	}
	stop() {
		this.changesets.close();
		this.dap.close();
		this.lsp.close();
		this.ahp.stop();
		this.store.disconnect();
	}
	backendOf(key: string): string {
		if (isDraft(key)) return draftBackend(key);
		return (
			this.providers.get(key) ??
			(this.store.getSnapshot()[key]?.provider ||
				canonicalBackend(key) ||
				"wingman")
		);
	}
	canFork(backend: string): boolean {
		return (
			this.ahp.root.agents.find((agent) => agent.provider === backend)
				?.capabilities?.multipleChats?.fork === true
		);
	}
	private remember(summary: SessionSummary) {
		const chats = summary.chats?.length
			? summary.chats.map((chat) => chat.resource)
			: [summary.defaultChat ?? sessionToChat(summary.resource)];
		for (const chat of chats) {
			this.sessions.set(chat, summary.resource);
			this.providers.set(chat, summary.provider);
		}
	}
	private async listSessions() {
		const result = await this.ahp.request("listSessions", {
			channel: "ahp-root://",
		});
		for (const summary of result.items) this.remember(summary);
		return result.items;
	}
	async sessionOf(chat: string): Promise<string> {
		chat = this.redirects.get(chat) ?? chat;
		const known = this.sessions.get(chat);
		if (known) return known;
		if (canonicalBackend(chat) === undefined) {
			this.catalogue ??= this.listSessions()
				.then(() => {})
				.finally(() => {
					this.catalogue = undefined;
				});
			await this.catalogue;
		}
		// A default chat shares its session's path.
		return this.sessions.get(chat) ?? chatToSession(chat);
	}
	private async redirect(from: string, alias: string) {
		const to = alias.startsWith("ahp-session:")
			? ((await this.ahp.read<SessionState>(alias)).defaultChat ??
				sessionToChat(alias))
			: alias;
		if (this.redirects.get(from) === to) return;
		this.redirects.set(from, to);
		if (alias.startsWith("ahp-session:")) this.sessions.set(to, alias);
		this.announce(from, to);
	}
	private announce(from: string, to: string) {
		for (const listener of this.redirectListeners) listener(from, to);
	}
	watch(key: string): () => void {
		if (isDraft(key)) return () => {};
		let entry = this.observed.get(key);
		if (entry) entry.count++;
		else {
			entry = { count: 1, release: () => {} };
			this.observed.set(key, entry);
			this.store.expect(key);
			const current = entry;
			let resolving = false,
				attached = false;
			let releaseChannels = () => {};
			const live = () => this.observed.get(key) === current;
			const failed = (error: unknown, chat = false) => {
				const alias = aliasOf(error);
				if (alias && live()) {
					void this.redirect(key, alias).catch(() => {});
					return;
				}
				// The chat's URI is never derived, so the host not knowing it means it is gone.
				if (chat && error instanceof RpcError && error.code === -32001) {
					deleted();
					return;
				}
				if (live() && this.store.getSnapshot()[key]?.status !== "deleted")
					this.store.set(key, {
						...emptySession(key),
						status: "error",
						error: String(error),
					});
			};
			const deleted = () => {
				if (live())
					this.store.set(key, { ...emptySession(key), status: "deleted" });
			};
			const connect = () => {
				if (resolving || attached) return;
				const to = this.redirects.get(key);
				if (to) {
					// Subscribing a key the host already moved would report it gone.
					attached = true;
					queueMicrotask(() => {
						if (live()) this.announce(key, to);
					});
					return;
				}
				if (!this.ahp.getConnected()) return;
				resolving = true;
				void this.sessionOf(key)
					.then((uri) => {
						if (!live()) return;
						attached = true;
						let session: SessionState | undefined, chat: ChatState | undefined;
						let sessionConnection = -1,
							chatConnection = -2,
							revision = 0;
						const projection = new AHPSessionProjection(this.ahp.directory);
						const update = () => {
							if (session && chat && sessionConnection === chatConnection)
								this.store.set(
									key,
									projection.project(key, session, chat, ++revision, {
										uri,
										root: this.ahp.root,
									}),
								);
						};
						const releaseSession = this.ahp.watch(
							uri,
							(event) => {
								session = event.state as SessionState;
								sessionConnection = event.connection;
								this.providers.set(key, session.provider);
								for (const summary of session.chats)
									this.sessions.set(summary.resource, uri);
								// A disposed fork leaves the session's chat list.
								if (
									session.chats.length &&
									!session.chats.some((summary) => summary.resource === key)
								)
									deleted();
								else update();
							},
							failed,
						);
						const releaseChat = this.ahp.watch(
							key,
							(event) => {
								chat = event.state as ChatState;
								chatConnection = event.connection;
								update();
							},
							(error) => failed(error, true),
						);
						// Disposal is announced on the root channel only.
						const releaseRemoval = this.ahp.onNotification((method, params) => {
							if (method === "root/sessionRemoved" && params?.session === uri)
								deleted();
						});
						releaseChannels = () => {
							releaseSession();
							releaseChat();
							releaseRemoval();
						};
					})
					.catch(failed)
					.finally(() => {
						resolving = false;
					});
			};
			// React child effects may subscribe before the provider starts the connection.
			// A stopped or reconnecting transport keeps the subscription intent alive.
			const releaseConnection = this.ahp.subscribeConnection(connect);
			current.release = () => {
				releaseConnection();
				releaseChannels();
			};
			connect();
		}
		const current = entry;
		let released = false;
		return () => {
			if (released) return;
			released = true;
			if (this.observed.get(key) !== current || --current.count > 0) return;
			current.release();
			this.observed.delete(key);
			this.store.forget(key);
		};
	}
	private chat(key: string) {
		return this.ahp.read<ChatState>(this.redirects.get(key) ?? key);
	}
	private message(
		text: string,
		files: string[] = [],
		images: string[] = [],
		completions: MessageAttachment[] = [],
	): Message {
		const attachments: MessageAttachment[] = files.map((file) => ({
			type: MessageAttachmentKind.Resource,
			label: file,
			uri: new URL(
				file.split("/").map(encodeURIComponent).join("/"),
				this.ahp.directory.replace(/\/$/, "") + "/",
			).href,
		}));
		for (const image of images) {
			const match = /^data:(image\/[^;]+);base64,(.*)$/.exec(image);
			if (match)
				attachments.push({
					type: MessageAttachmentKind.EmbeddedResource,
					label: "Image",
					contentType: match[1],
					data: match[2],
				});
		}
		return {
			text,
			attachments: [...attachments, ...completions],
			origin: { kind: MessageKind.User },
		};
	}
	/** Stopping a turn also drops the queue; the host removes queued messages. */
	async cancel(key: string) {
		const chat = await this.chat(key);
		const turnId = chat.activeTurn?.id;
		if (!turnId) {
			await this.clearQueue(key);
			return;
		}
		try {
			await this.ahp.dispatch(chat.resource, {
				type: ActionType.ChatTurnCancelled,
				turnId,
				duration: 0,
			});
		} catch (error) {
			// Stopping succeeded if the turn ended first.
			const current = await this.chat(key);
			if (current.activeTurn?.id === turnId) throw error;
		}
	}
	async clearQueue(key: string) {
		const chat = await this.chat(key);
		const pending = [
			...(chat.steeringMessage ? [chat.steeringMessage] : []),
			...(chat.queuedMessages ?? []),
		];
		await Promise.all(
			pending.map((input) =>
				this.ahp.dispatch(chat.resource, {
					type: ActionType.ChatPendingMessageRemoved,
					kind:
						input === chat.steeringMessage
							? PendingMessageKind.Steering
							: (input.kind ?? PendingMessageKind.Queued),
					id: input.id,
				}),
			),
		);
	}
	/** Compaction is a turn the host runs for a `/compact [focus]` message. */
	compact(key: string, focus = "") {
		return this.send(key, focus ? `/compact ${focus}` : "/compact");
	}
	async removeQueued(key: string, inputId: string) {
		const chat = await this.chat(key);
		const kind =
			chat.queuedMessages?.find((input) => input.id === inputId)?.kind ??
			(chat.steeringMessage?.id === inputId
				? PendingMessageKind.Steering
				: PendingMessageKind.Queued);
		await this.ahp.dispatch(chat.resource, {
			type: ActionType.ChatPendingMessageRemoved,
			kind,
			id: inputId,
		});
	}
	async updateQueued(
		key: string,
		inputId: string,
		text: string,
		files: string[] = [],
		images: string[] = [],
		completions: MessageAttachment[] = [],
	) {
		const chat = await this.chat(key);
		const steering =
			chat.steeringMessage?.id === inputId ||
			chat.queuedMessages?.find((input) => input.id === inputId)?.kind ===
				PendingMessageKind.Steering;
		await this.ahp.dispatch(chat.resource, {
			type: ActionType.ChatPendingMessageSet,
			kind: steering ? PendingMessageKind.Steering : PendingMessageKind.Queued,
			id: inputId,
			message: this.message(text, files, images, completions),
		});
	}
	async respond(key: string, promptId: string, reply: PromptReply) {
		const chat = await this.chat(key);
		const turn = chat.activeTurn;
		const part = turn?.responseParts.find(
			(p) => p.kind === "toolCall" && p.toolCall.toolCallId === promptId,
		);
		if (turn && part?.kind === "toolCall") {
			const options =
				part.toolCall.status === "pending-confirmation"
					? (part.toolCall.options ?? [])
					: [];
			const kind = reply.action === "accept" ? "approve" : "deny";
			const option =
				options.find((candidate) => candidate.id === reply.option) ??
				options.find((candidate) => candidate.kind === kind);
			const approved = option
				? option.kind === "approve"
				: reply.action === "accept";
			await this.ahp.dispatch(chat.resource, {
				type: ActionType.ChatToolCallConfirmed,
				turnId: turn.id,
				toolCallId: promptId,
				...(approved
					? {
							approved: true,
							confirmed: ToolCallConfirmationReason.UserAction,
						}
					: { approved: false, reason: ToolCallCancellationReason.Denied }),
				...(option ? { selectedOptionId: option.id } : {}),
			});
		} else
			await this.ahp.dispatch(chat.resource, {
				type: ActionType.ChatInputCompleted,
				requestId: promptId,
				response: reply.action as ChatInputResponseKind,
				answers: inputAnswers(
					chat,
					promptId,
					(reply.action === "accept" && reply.content) || {},
				),
			});
	}
	async configure(key: string, settings: SettingsPatch) {
		const chat = await this.chat(key);
		const config: Record<string, unknown> = {};
		for (const [name, value] of Object.entries(settings))
			if (value !== undefined) config[name] = value;
		await this.ahp.dispatch(chat.resource, {
			type: ActionType.ChatConfigChanged,
			config,
		});
	}
	/** Disposing a session's default chat disposes the session with its forks. */
	async dispose(key: string) {
		const uri = await this.sessionOf(key);
		const session = await this.ahp.read<SessionState>(uri);
		if (session.defaultChat && session.defaultChat !== key)
			await this.ahp.request("disposeChat", { channel: key });
		else await this.ahp.request("disposeSession", { channel: uri });
	}
	async send(
		key: string,
		text: string,
		files: string[] = [],
		images: string[] = [],
		intent = "follow_up",
		completions: MessageAttachment[] = [],
	) {
		// The session phase may change before the user retries, so intent must not
		// split the retry identity; a retry keeps the original request.
		const fingerprint = JSON.stringify([key, text, files, images, completions]);
		let entry = this.sends.get(fingerprint);
		if (!entry) {
			entry = {
				request: {
					id: crypto.randomUUID(),
					text,
					files: [...files],
					images: [...images],
					completions: structuredClone(completions),
					intent,
				},
			};
			this.sends.set(fingerprint, entry);
		}
		if (!entry.pending) {
			const current = entry;
			current.pending = this.deliver(key, current.request)
				.then(() => {
					this.sends.delete(fingerprint);
				})
				.finally(() => {
					current.pending = undefined;
				});
		}
		await entry.pending;
	}
	private async deliver(key: string, request: SendRequest) {
		const chat = await this.chat(key);
		await this.ahp.dispatch(
			chat.resource,
			{
				type: ActionType.ChatPendingMessageSet,
				kind:
					request.intent === "steer"
						? PendingMessageKind.Steering
						: PendingMessageKind.Queued,
				id: request.id,
				message: this.message(
					request.text,
					request.files,
					request.images,
					request.completions,
				),
			},
			request.id,
		);
	}
	/** Creates the session a draft becomes and returns its default chat. */
	create(backend: string, draftID: string): Promise<string> {
		const key = JSON.stringify([backend, draftID]);
		let entry = this.creations.get(key);
		if (!entry) {
			entry = { uri: `ahp-session:/${crypto.randomUUID()}` };
			this.creations.set(key, entry);
		}
		if (!entry.pending) {
			const current = entry;
			current.pending = this.createAt(backend, current.uri).catch((error) => {
				current.pending = undefined;
				throw error;
			});
		}
		return entry.pending!;
	}
	private async createAt(backend: string, uri: string): Promise<string> {
		try {
			await this.ahp.request("createSession", {
				channel: uri,
				provider: backend,
			});
		} catch (error) {
			if (!(error instanceof RpcError && error.code === -32003)) throw error;
		}
		const session = await this.ahp.read<SessionState>(uri);
		const chat = session.defaultChat ?? sessionToChat(uri);
		this.sessions.set(chat, uri);
		this.providers.set(chat, backend);
		return chat;
	}
	/** Forks a chat through a completed turn, the latest one by default. */
	async fork(key: string, title?: string, turnId?: string): Promise<string> {
		const fingerprint = JSON.stringify([key, title, turnId]);
		let entry = this.forks.get(fingerprint);
		if (!entry) {
			entry = { uri: `ahp-chat:/${crypto.randomUUID()}` };
			this.forks.set(fingerprint, entry);
		}
		if (!entry.pending) {
			const current = entry;
			current.pending = this.createFork(key, current.uri, title, turnId)
				.then((result) => {
					this.forks.delete(fingerprint);
					return result;
				})
				.finally(() => {
					current.pending = undefined;
				});
		}
		return entry.pending!;
	}
	private async createFork(
		key: string,
		uri: string,
		title?: string,
		turnId?: string,
	) {
		const session = await this.sessionOf(key);
		const source = await this.chat(key);
		const turn = turnId ?? source.turns.at(-1)?.id;
		if (!turn) throw new Error("There is no completed turn to fork from");
		try {
			await this.ahp.request("createChat", {
				channel: session,
				chat: uri,
				source: {
					kind: ChatSourceKind.Fork,
					chat: source.resource,
					turnId: turn,
				},
				...(title ? { title } : {}),
			});
		} catch (error) {
			// The chat URI is minted here, so an existing chat is this fork's earlier attempt.
			const exists =
				error instanceof RpcError &&
				(error.code === -32010 || error.code === -32003);
			if (!exists) throw error;
		}
		this.sessions.set(uri, session);
		this.providers.set(uri, this.backendOf(key));
		return uri;
	}
	async list(backend: string): Promise<SessionInfo[]> {
		// Resolve config starts a lazily loaded provider before cataloguing it.
		await this.ahp.request("resolveSessionConfig", {
			channel: "ahp-root://",
			provider: backend,
		});
		const items = await this.listSessions();
		return items
			.filter((summary) => summary.provider === backend)
			.map((summary) => {
				const fallback = summary.defaultChat ?? sessionToChat(summary.resource);
				const chats = summary.chats?.length
					? summary.chats
					: [{ resource: fallback, title: summary.title }];
				const first =
					chats.find((chat) => chat.resource === summary.defaultChat) ??
					chats[0];
				return {
					id: first.resource,
					session: summary.resource,
					title: summary.title,
					updated_at: summary.modifiedAt,
					chats: [first, ...chats.filter((chat) => chat !== first)].map(
						(chat) => ({ id: chat.resource, title: chat.title }),
					),
				};
			});
	}
}

let client: WorkspaceClient;
export function initializeWorkspace(scope: Bootstrap) {
	client?.stop();
	client = new WorkspaceClient(scope);
	setWorkspaceHTTP(client.http);
	client.start();
	return client;
}
export function workspaceClient(): WorkspaceClient {
	return client;
}
