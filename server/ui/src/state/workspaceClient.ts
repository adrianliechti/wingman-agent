import type { WorkspaceInfo } from "../api/workspace.ts";
import { ChangesetClient } from "./changesetClient.ts";
import { FileClient } from "./fileClient.ts";
import { DAPClient } from "./dapClient.ts";
import { LSPClient } from "./lspClient.ts";
import {
	MessageKind,
	type StateAction,
	type ChatState,
	type SessionState,
	type Message,
	type MessageAttachment,
	type ChatInputResponseKind,
} from "@microsoft/agent-host-protocol";
import {
	ActionType,
	PendingMessageKind,
	MessageAttachmentKind,
} from "./ahpValues.ts";
import { RpcError } from "@microsoft/agent-host-protocol/client";
import { AHPConnection } from "./ahpConnection.ts";
import {
	AHPSessionProjection,
	runtimeInfo,
	inputAnswers,
} from "./ahpSession.ts";
import {
	APIError,
	createWorkspaceHTTP,
	setWorkspaceHTTP,
} from "../api/http.ts";
import type { ServerMessage } from "../types/protocol.ts";
import {
	SessionStore,
	sessionKey,
	splitSessionKey,
	emptySession,
	type SessionRef,
	type WorkspaceScope,
} from "./sessionStore.ts";

export type Bootstrap = WorkspaceScope & {
	protocol: number;
	backends: { id: string; name: string }[];
};
export async function fetchWorkspace(): Promise<WorkspaceInfo & Bootstrap> {
	const response = await fetch("/api/workspace");
	if (!response.ok) throw new APIError(await response.text(), response.status);
	const scope = (await response.json()) as WorkspaceInfo;
	if (scope.protocol !== 2 || !scope.instanceId || !scope.workspaceId)
		throw new Error("Unsupported workspace protocol. Reload the application.");
	const backends = await createWorkspaceHTTP(scope.instanceId).fetchJSON<
		Bootstrap["backends"]
	>("/api/v2/backends");
	if (!Array.isArray(backends)) throw new Error("Invalid backend catalogue");
	return { ...scope, backends };
}

export type Receipt = {
	id: string;
	ref: SessionRef;
	epoch: string;
	outcome: string;
};
export type Command = {
	type: string;
	id?: string;
	epoch?: string;
	inputId?: string;
	[key: string]: unknown;
};
export function sessionPath(key: string): string {
	const ref = splitSessionKey(key);
	return `/api/v2/backends/${encodeURIComponent(ref.backendId)}/sessions/${encodeURIComponent(ref.sessionId)}`;
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
	private resources = new Map<string, string>();
	private sends = new Map<
		string,
		{ command: Command; pending?: Promise<Receipt> }
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
	readonly getConnected = () => this.ahp.getConnected();
	readonly subscribeConnection = (listener: () => void) =>
		this.ahp.subscribeConnection(listener);
	readonly subscribe = (listener: (event: ServerMessage) => void) =>
		this.ahp.subscribe(listener);
	start() {
		this.ahp.start();
	}
	stop() {
		this.changesets.close();
		this.dap.close();
		this.lsp.close();
		this.ahp.stop();
		this.store.disconnect();
	}
	async resource(key: string): Promise<string> {
		const known = this.resources.get(key);
		if (known) return known;
		const ref = splitSessionKey(key);
		const result = await this.ahp.extension<{ resource: string }>(
			"x-wingman/resolveSession",
			{
				channel: "ahp-root://",
				backend: ref.backendId,
				session: ref.sessionId,
			},
		);
		this.resources.set(key, result.resource);
		return result.resource;
	}
	watch(key: string): () => void {
		if (!splitSessionKey(key).sessionId) return () => {};
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
			const connect = () => {
				if (!this.ahp.getConnected() || resolving || attached) return;
				resolving = true;
				void this.resource(key)
					.then((uri) => {
						if (this.observed.get(key) !== current) return;
						attached = true;
						let session: SessionState | undefined, chat: ChatState | undefined;
						let sessionConnection = -1,
							chatConnection = -2;
						let chatURI = "",
							revision = 0,
							releaseChat = () => {};
						const projection = new AHPSessionProjection();
						const update = () => {
							if (session && chat && sessionConnection === chatConnection)
								this.store.set(
									key,
									projection.project(key, session, chat, ++revision),
								);
						};
						const releaseSession = this.ahp.watch(uri, (event) => {
							session = event.state as SessionState;
							sessionConnection = event.connection;
							if (session.defaultChat && session.defaultChat !== chatURI) {
								releaseChat();
								chat = undefined;
								chatURI = session.defaultChat;
								releaseChat = this.ahp.watch(chatURI, (next) => {
									chat = next.state as ChatState;
									chatConnection = next.connection;
									update();
								});
							}
							update();
						});
						releaseChannels = () => {
							releaseSession();
							releaseChat();
						};
					})
					.catch((error) => {
						if (this.observed.get(key) === current)
							this.store.set(key, {
								...emptySession(key),
								status: "error",
								error: String(error),
							});
					})
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
	private async state(key: string) {
		const uri = await this.resource(key);
		const session = await this.ahp.read<SessionState>(uri);
		if (!session.defaultChat) throw new Error("Session has no chat");
		const chat = await this.ahp.read<ChatState>(session.defaultChat);
		return { uri, session, chat, info: runtimeInfo(session) };
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
	async command(key: string, command: Command): Promise<Receipt> {
		const { uri, chat, info } = await this.state(key);
		if (command.epoch && command.epoch !== info.epoch)
			throw new Error("Session changed. Reload before retrying.");
		const id = command.id ?? crypto.randomUUID();
		let action: StateAction | undefined,
			channel = chat.resource;
		switch (command.type) {
			case "send":
			case "queue_update":
				action = {
					type: ActionType.ChatPendingMessageSet,
					kind:
						command.intent === "steer"
							? PendingMessageKind.Steering
							: PendingMessageKind.Queued,
					id: command.inputId ?? id,
					message: this.message(
						String(command.text ?? ""),
						command.files as string[],
						command.images as string[],
						command.completions as MessageAttachment[],
					),
				};
				break;
			case "queue_remove":
				action = {
					type: ActionType.ChatPendingMessageRemoved,
					kind:
						chat.queuedMessages?.find((input) => input.id === command.inputId)
							?.kind ??
						(chat.steeringMessage?.id === command.inputId
							? PendingMessageKind.Steering
							: PendingMessageKind.Queued),
					id: command.inputId!,
				};
				break;
			case "cancel":
				if (command.clearQueue)
					await this.ahp.extension("x-wingman/queueControl", {
						channel: uri,
						id,
						epoch: info.epoch,
						operation: "cancelAll",
					});
				else if (chat.activeTurn)
					action = {
						type: ActionType.ChatTurnCancelled,
						turnId: chat.activeTurn.id,
						duration: 0,
					};
				break;
			case "settings": {
				channel = uri;
				const config: Record<string, unknown> = {};
				for (const name of ["model", "effort", "mode", "unattended"])
					if (command[name] !== undefined) config[name] = command[name];
				action = { type: ActionType.SessionConfigChanged, config };
				break;
			}
			case "prompt_response": {
				const promptId = String(command.promptId);
				const part = chat.activeTurn?.responseParts.find(
					(p) =>
						p.kind === "toolCall" &&
						p.toolCall.toolCallId === `approval:${promptId}`,
				);
				if (part && chat.activeTurn)
					action = {
						type: ActionType.ChatToolCallConfirmed,
						turnId: chat.activeTurn.id,
						toolCallId: `approval:${promptId}`,
						...(command.action === "accept"
							? {
									approved: true,
									confirmed:
										"user-action" as import("@microsoft/agent-host-protocol").ToolCallConfirmationReason.UserAction,
								}
							: {
									approved: false,
									reason:
										"denied" as import("@microsoft/agent-host-protocol").ToolCallCancellationReason.Denied,
								}),
						_meta: { "wingman.scope": command.scope ?? "once" },
					};
				else
					action = {
						type: ActionType.ChatInputCompleted,
						requestId: promptId,
						response: command.action as ChatInputResponseKind,
						answers: inputAnswers(
							chat,
							promptId,
							(command.content ?? {}) as Record<string, unknown>,
						),
					};
				break;
			}
			case "delete":
				await this.ahp.request("disposeSession", { channel: uri });
				break;
			case "compact":
				await this.ahp.extension("x-wingman/compact", {
					channel: uri,
					id,
					epoch: info.epoch,
					text: command.text ?? "",
				});
				break;
			case "queue_clear":
			case "queue_resume":
				await this.ahp.extension("x-wingman/queueControl", {
					channel: uri,
					id,
					epoch: info.epoch,
					operation: command.type === "queue_clear" ? "clear" : "resume",
				});
				break;
			default:
				throw new Error(`Unsupported session operation: ${command.type}`);
		}
		if (action) await this.ahp.dispatch(channel, action, id);
		return { id, ref: info.ref, epoch: info.epoch, outcome: "accepted" };
	}
	async send(
		key: string,
		text: string,
		files: string[] = [],
		images: string[] = [],
		intent = "follow_up",
		completions: MessageAttachment[] = [],
	) {
		const fingerprint = JSON.stringify([
			key,
			text,
			files,
			images,
			intent,
			completions,
		]);
		let entry = this.sends.get(fingerprint);
		if (!entry) {
			entry = {
				command: {
					type: "send",
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
			current.pending = this.command(key, current.command)
				.then((receipt) => {
					this.sends.delete(fingerprint);
					return receipt;
				})
				.finally(() => {
					current.pending = undefined;
				});
		}
		await entry.pending;
	}
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
	private async createAt(
		backend: string,
		uri: string,
		fork?: { source: string; title?: string },
	): Promise<string> {
		try {
			await this.ahp.request("createSession", {
				channel: uri,
				provider: backend,
				...(fork ? { _meta: { "wingman.fork": fork } } : {}),
			});
		} catch (error) {
			if (!(error instanceof RpcError && error.code === -32003)) throw error;
		}
		const session = await this.ahp.read<SessionState>(uri),
			info = runtimeInfo(session);
		const key = sessionKey(info.ref.backendId, info.ref.sessionId);
		this.resources.set(key, uri);
		return key;
	}
	async fork(key: string, title?: string): Promise<string> {
		const fingerprint = JSON.stringify([key, title]),
			ref = splitSessionKey(key);
		let entry = this.forks.get(fingerprint);
		if (!entry) {
			entry = { uri: `ahp-session:/${crypto.randomUUID()}` };
			this.forks.set(fingerprint, entry);
		}
		if (!entry.pending) {
			const current = entry;
			current.pending = this.resource(key)
				.then((source) =>
					this.createAt(ref.backendId, current.uri, { source, title }),
				)
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
	async list(backend: string) {
		// Resolve config starts a lazily loaded provider before cataloguing it.
		await this.ahp.request("resolveSessionConfig", {
			channel: "ahp-root://",
			provider: backend,
		});
		const result = await this.ahp.request("listSessions", {
			channel: "ahp-root://",
		});
		return result.items
			.filter((s) => s.provider === backend)
			.map((summary) => {
				const ref = summary._meta?.["wingman.ref"] as SessionRef | undefined;
				if (!ref)
					throw new Error("Session catalog is missing its native identity");
				const key = sessionKey(ref.backendId, ref.sessionId);
				this.resources.set(key, summary.resource);
				return {
					id: key,
					title: summary.title,
					updated_at: summary.modifiedAt,
				};
			});
	}
	focus() {
		void this.ahp
			.extension("x-wingman/focus", { channel: "ahp-root://" })
			.catch(() => {});
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
