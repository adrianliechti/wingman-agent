import type { WorkspaceInfo } from "../api/workspace.ts";
import { ChangesetClient } from "./changesetClient.ts";
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
import { errorText } from "../utils/errorText.ts";
import { readStored, writeStored } from "../utils/storage.ts";
import { AHPConnection } from "./ahpConnection.ts";
import {
	AHPSessionProjection,
	inputAnswers,
	inputRequest,
} from "./ahpSession.ts";
import {
	APIError,
	createWorkspaceHTTP,
	setWorkspaceHTTP,
} from "../api/http.ts";
import type { PromptReply, ServerMessage } from "../types/protocol.ts";
import { FleetViewStore } from "./fleet.ts";
import { SessionCatalog } from "./sessionCatalog.ts";
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

/** The namespace is where the host names this project's channels. */
export type Bootstrap = WorkspaceScope & {
	protocol: number;
	namespace: string;
};
/** The project a window shows, which its address names; none is the host's start folder. */
export function windowProject(): string | null {
	return new URLSearchParams(location.search).get("project");
}
/** The address of a project's window, opening a chat there when one is given. */
export function projectHref(
	project: string,
	chat?: { backend: string; id: string },
): string {
	const path = chat
		? `/${encodeURIComponent(chat.backend)}/${encodeURIComponent(chat.id)}`
		: "/";
	return `${path}?project=${encodeURIComponent(project)}`;
}
/** Asks the host for a project's window, which opens the project there. */
async function requestWorkspace(
	project: string | null,
	signal?: AbortSignal,
): Promise<Response> {
	const response = await fetch(
		project
			? `/api/workspace?project=${encodeURIComponent(project)}`
			: "/api/workspace",
		{ signal },
	);
	if (response.ok) return response;
	const text = (await response.text()).trim();
	throw new APIError(
		// The host's own words for a missing folder are a system call's.
		response.status === 404 && project
			? "Its folder no longer exists."
			: text || `HTTP ${response.status}`,
		response.status,
	);
}
/** Opens a project on the host before a window moves there: one that cannot
 * open, such as a folder that is gone, leaves the window where it is. */
export async function openProject(
	project: string,
	signal?: AbortSignal,
): Promise<void> {
	await requestWorkspace(project, signal);
}
export async function fetchWorkspace(): Promise<WorkspaceInfo & Bootstrap> {
	const response = await requestWorkspace(windowProject());
	const scope = (await response.json()) as WorkspaceInfo;
	if (
		scope.protocol !== 2 ||
		!scope.instanceId ||
		!scope.workspaceId ||
		!scope.namespace
	)
		throw new Error("Unsupported workspace protocol. Reload the application.");
	return scope;
}

type SendRequest = {
	id: string;
	text: string;
	files: string[];
	images: string[];
	intent: string;
	attachments: MessageAttachment[];
};
type Send = { request: SendRequest; pending?: Promise<void> };
const same = (a: readonly unknown[], b: readonly unknown[]) =>
	a.length === b.length && a.every((value, i) => value === b[i]);

const FLEET_VIEW_KEY = "wingman.fleetView";

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
	/** The host's sessions, kept current from its root notifications. */
	readonly catalog: SessionCatalog;
	/** What the session list shows, kept for the window's next page. */
	readonly fleetView: FleetViewStore;
	private observed = new Map<string, { count: number; release: () => void }>();
	/** Chat URI to the URI of the session that owns it. */
	private sessions = new Map<string, string>();
	private providers = new Map<string, string>();
	private redirects = new Map<string, string>();
	private redirectListeners = new Set<(from: string, to: string) => void>();
	/** Each chat's latest send, kept after a failure so a retry reuses its request. */
	private sends = new Map<string, Send>();
	private forks = new Map<string, { uri: string; pending?: Promise<string> }>();
	private creations = new Map<
		string,
		{ uri: string; pending?: Promise<string> }
	>();
	constructor(scope: Bootstrap, ahp = new AHPConnection(scope)) {
		this.lsp = new LSPClient(scope.instanceId);
		this.dap = new DAPClient(scope.instanceId);
		this.files = new FileClient(ahp, this.lsp);
		this.changesets = new ChangesetClient(ahp, scope.namespace);
		this.scope = scope;
		this.ahp = ahp;
		this.catalog = new SessionCatalog(ahp, (summary) => this.remember(summary));
		this.fleetView = new FleetViewStore(
			{
				// A browser without storage starts each window's list afresh, and
				// a view it does not keep still holds for the page.
				read: () => readStored("session", FLEET_VIEW_KEY),
				write: (view) => void writeStored("session", FLEET_VIEW_KEY, view),
			},
			scope.project,
		);
		this.http = createWorkspaceHTTP(scope.instanceId);
		ahp.subscribeConnection(() => {
			if (ahp.getConnected()) return;
			this.store.disconnect();
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
		this.changesets.watch(this.changesets.layer());
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
	async sessionOf(chat: string): Promise<string> {
		chat = this.redirects.get(chat) ?? chat;
		const known = this.sessions.get(chat);
		if (known) return known;
		if (canonicalBackend(chat) === undefined) await this.catalog.read();
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
						error: errorText(error),
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
								// A disposed chat leaves the session's chat list; a disposed session lists none.
								if (!session.chats.some((summary) => summary.resource === key))
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
						// A session's config schema follows its agent's models without an action.
						const releaseAgents = this.ahp.watch("ahp-root://", (event) => {
							if (event.action?.action.type === "root/agentsChanged")
								this.ahp.refresh(uri);
						});
						releaseChannels = () => {
							releaseSession();
							releaseChat();
							releaseAgents();
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
		attachments: MessageAttachment[] = [],
	): Message {
		const encoded: MessageAttachment[] = files.map((file) => ({
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
				encoded.push({
					type: MessageAttachmentKind.EmbeddedResource,
					label: "Image",
					contentType: match[1],
					data: match[2],
				});
		}
		return {
			text,
			attachments: [...encoded, ...attachments],
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
		// Protocol 1.1 lists steering messages in the queue as well.
		await Promise.all(
			(chat.queuedMessages ?? []).map((input) =>
				this.ahp.dispatch(chat.resource, {
					type: ActionType.ChatPendingMessageRemoved,
					kind: input.kind ?? PendingMessageKind.Queued,
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
			PendingMessageKind.Queued;
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
		attachments: MessageAttachment[] = [],
	) {
		const chat = await this.chat(key);
		const steering =
			chat.queuedMessages?.find((input) => input.id === inputId)?.kind ===
			PendingMessageKind.Steering;
		await this.ahp.dispatch(chat.resource, {
			type: ActionType.ChatPendingMessageSet,
			kind: steering ? PendingMessageKind.Steering : PendingMessageKind.Queued,
			id: inputId,
			message: this.message(text, files, images, attachments),
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
		} else {
			const session = await this.ahp.read<SessionState>(
				await this.sessionOf(key),
			);
			await this.ahp.dispatch(chat.resource, {
				type: ActionType.ChatInputCompleted,
				requestId: promptId,
				response: reply.action as ChatInputResponseKind,
				answers: inputAnswers(
					inputRequest(session, chat, promptId),
					(reply.action === "accept" && reply.content) || {},
				),
			});
		}
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
		this.sends.delete(key);
		const uri = await this.sessionOf(key);
		// The list tells a session's own chat from a fork, so a listed session
		// is deleted without loading it: its folder may be gone.
		const listed = this.catalog.summary(uri);
		let own: boolean;
		if (listed) own = (listed.defaultChat ?? sessionToChat(uri)) === key;
		else {
			const session = await this.ahp.read<SessionState>(uri);
			own = !session.defaultChat || session.defaultChat === key;
		}
		if (!own) {
			await this.ahp.request("disposeChat", { channel: key });
			return;
		}
		await this.ahp.request("disposeSession", { channel: uri });
		this.catalog.forget(uri);
	}
	/** Marks a chat read or unread, or a session with all its chats. */
	markRead(uri: string, read: boolean): Promise<void> {
		return this.ahp.dispatch(
			uri,
			uri.startsWith("ahp-session:")
				? { type: ActionType.SessionIsReadChanged, isRead: read }
				: { type: ActionType.ChatIsReadChanged, isRead: read },
		);
	}
	/** Archived sessions leave the list until they need the user. */
	archive(session: string, archived: boolean): Promise<void> {
		return this.ahp.dispatch(session, {
			type: ActionType.SessionIsArchivedChanged,
			isArchived: archived,
		});
	}
	async send(
		key: string,
		text: string,
		files: string[] = [],
		images: string[] = [],
		intent = "follow_up",
		attachments: MessageAttachment[] = [],
	) {
		// The session phase may change before the user retries, so intent must not
		// split the retry identity; a retry keeps the original request.
		const previous = this.sends.get(key);
		const retry =
			previous?.request.text === text &&
			same(previous.request.files, files) &&
			same(previous.request.images, images) &&
			JSON.stringify(previous.request.attachments) ===
				JSON.stringify(attachments);
		const entry: Send = retry
			? previous
			: {
					request: {
						id: crypto.randomUUID(),
						text,
						files: [...files],
						images: [...images],
						attachments: structuredClone(attachments),
						intent,
					},
				};
		this.sends.set(key, entry);
		entry.pending ??= this.deliver(key, entry.request)
			.then(() => {
				if (this.sends.get(key) === entry) this.sends.delete(key);
			})
			.finally(() => {
				entry.pending = undefined;
			});
		await entry.pending;
	}
	private async deliver(key: string, request: SendRequest) {
		const chat = await this.chat(key);
		const message = this.message(
			request.text,
			request.files,
			request.images,
			request.attachments,
		);
		const steer = request.intent === "steer";
		await this.ahp.dispatch(
			chat.resource,
			{
				type: ActionType.ChatPendingMessageSet,
				kind: steer ? PendingMessageKind.Steering : PendingMessageKind.Queued,
				id: request.id,
				message,
			},
			request.id,
		);
		// Steering that missed its turn waits for the next one; the user meant to be answered now.
		const now = await this.chat(key);
		if (
			steer &&
			!now.activeTurn &&
			now.queuedMessages?.some(
				(m) => m.id === request.id && m.kind === PendingMessageKind.Steering,
			)
		)
			await this.ahp.dispatch(now.resource, {
				type: ActionType.ChatPendingMessageSet,
				kind: PendingMessageKind.Queued,
				id: request.id,
				message,
			});
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
				// The host's default project is only the start folder's window.
				...(this.ahp.directory
					? { workingDirectories: [this.ahp.directory] }
					: {}),
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
