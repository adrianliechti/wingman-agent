import {
	chatReducer,
	changesetReducer,
	type ChangesetState,
	type ChangesetAction,
	sessionReducer,
	terminalReducer,
	rootReducer,
	type ActionEnvelope,
	type StateAction,
	type Snapshot,
	type ChatState,
	type SessionState,
	type TerminalState,
	type RootState,
	type ChatAction,
	type SessionAction,
	type TerminalAction,
	type RootAction,
	type CommandMap,
} from "@microsoft/agent-host-protocol";
import {
	AhpClient,
	type AhpTransport,
} from "@microsoft/agent-host-protocol/client";
import { WebSocketTransport } from "@microsoft/agent-host-protocol/ws";
import type { ServerMessage } from "../types/protocol.ts";
import type { WorkspaceScope } from "./sessionStore.ts";

export type ChannelState =
	| ChatState
	| SessionState
	| TerminalState
	| RootState
	| ChangesetState;
export type ChannelUpdate = {
	state: ChannelState;
	action?: ActionEnvelope;
	snapshot: boolean;
	connection: number;
};
type Observer = {
	listeners: Set<(update: ChannelUpdate) => void>;
	state?: ChannelState;
	seq: number;
	pending: ActionEnvelope[];
	generation: number;
};
type PendingAction = {
	channel: string;
	action: StateAction;
	seq: number;
	resolve?: () => void;
	reject?: (error: Error) => void;
	timer?: ReturnType<typeof setTimeout>;
};

/** One connection owns all browser AHP channels. The official SDK owns RPC and
 * wire types; its official reducers own authoritative channel state. We process
 * notifications directly at the transport boundary so a dropped SDK event-queue
 * item can never silently corrupt a long-running terminal or conversation. */
export class AHPConnection {
	readonly clientId = crypto.randomUUID();
	private client?: AhpClient;
	private generation = 0;
	private stopped = true;
	private connecting?: Promise<void>;
	private timer?: ReturnType<typeof setTimeout>;
	private sequence = 0;
	private actions = new Map<string, PendingAction>();
	private observers = new Map<string, Observer>();
	private notifications = new Set<(method: string, params: any) => void>();
	onNotification(listener: (method: string, params: any) => void) {
		this.notifications.add(listener);
		return () => {
			this.notifications.delete(listener);
		};
	}
	private connectionListeners = new Set<() => void>();
	private eventListeners = new Set<(event: ServerMessage) => void>();
	private connected = false;
	private replaced = false;
	directory = "";
	root: RootState = { agents: [] };
	readonly scope: WorkspaceScope;
	private readonly dial: () => Promise<AhpTransport>;
	constructor(
		scope: WorkspaceScope,
		dial: () => Promise<AhpTransport> = () =>
			WebSocketTransport.connect(
				`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/ahp`,
			),
	) {
		this.scope = scope;
		this.dial = dial;
	}
	readonly getConnected = () => this.connected;
	readonly getReplaced = () => this.replaced;
	readonly subscribeConnection = (fn: () => void) => {
		this.connectionListeners.add(fn);
		return () => {
			this.connectionListeners.delete(fn);
		};
	};
	readonly subscribe = (fn: (event: ServerMessage) => void) => {
		this.eventListeners.add(fn);
		return () => {
			this.eventListeners.delete(fn);
		};
	};
	private emit(event: ServerMessage) {
		for (const fn of this.eventListeners) fn(event);
	}
	private status(connected: boolean) {
		this.connected = connected;
		for (const fn of this.connectionListeners) fn();
	}
	start() {
		if (!this.stopped) return;
		this.stopped = false;
		void this.ready().catch(() => {});
	}
	stop() {
		this.stopped = true;
		++this.generation;
		this.connecting = undefined;
		clearTimeout(this.timer);
		void this.client?.shutdown();
		this.client = undefined;
		this.failActions(
			new Error("Workspace disconnected. Retry after reconnecting."),
		);
		this.failReads(new Error("Workspace client stopped."));
		this.status(false);
	}
	private failActions(error: Error) {
		for (const entry of this.actions.values()) {
			clearTimeout(entry.timer);
			entry.reject?.(error);
			entry.resolve = undefined;
			entry.reject = undefined;
		}
	}
	private failReads(error: Error) {
		for (const callbacks of [...this.channelErrors.values()])
			for (const fn of [...callbacks]) fn(error);
	}
	async ready(): Promise<void> {
		if (this.replaced)
			throw new Error("Workspace changed. Reload to reconnect.");
		if (this.stopped) throw new Error("Workspace client stopped.");
		if (this.connected) return;
		if (!this.connecting) {
			const connecting = this.connect().finally(() => {
				if (this.connecting === connecting) this.connecting = undefined;
			});
			this.connecting = connecting;
		}
		return this.connecting;
	}
	private async connect() {
		const generation = ++this.generation;
		let client: AhpClient | undefined;
		try {
			const transport = await this.dial();
			if (this.stopped || generation !== this.generation) {
				await transport.close();
				return;
			}
			const wrapped: AhpTransport = {
				send: (message) => transport.send(message),
				close: () => transport.close(),
				recv: async () => {
					const frame = await transport.recv();
					if (frame && generation === this.generation) {
						const message =
							frame.kind === "parsed"
								? frame.message
								: JSON.parse(
										frame.kind === "text"
											? frame.text
											: new TextDecoder().decode(frame.data),
									);
						if (message.method)
							for (const listener of this.notifications)
								listener(message.method, message.params);
						if (message.method === "action")
							this.action(message.params as ActionEnvelope);
						else if (message.method === "x-wingman/workspaceChanged")
							this.emit(message.params.event);
						else if (message.method?.startsWith("root/session"))
							this.emit({ type: "sessions_changed" });
					}
					return frame;
				},
			};
			client = new AhpClient(wrapped);
			this.client = client;
			const states = client.stateChanges();
			void (async () => {
				for await (const state of states) {
					if (state.status === "closed" && generation === this.generation) {
						this.client = undefined;
						this.status(false);
						this.failActions(
							new Error(
								"Connection lost. Delivery is unconfirmed; retry after reconnecting.",
							),
						);
						this.failReads(
							new Error("Connection lost. Retry after reconnecting."),
						);
						this.schedule();
					}
				}
			})();
			client.connect();
			const root = this.observe("ahp-root://");
			root.state = undefined;
			root.pending = [];
			root.generation = generation;
			const result = await client.initialize({
				clientId: this.clientId,
				protocolVersions: ["1.1.0", "1.0.0"],
				initialSubscriptions: ["ahp-root://"],
			});
			if (generation !== this.generation || this.stopped) return;
			const snapshot = result.snapshots?.find(
				(s) => s.resource === "ahp-root://",
			);
			if (!snapshot) throw new Error("Host did not provide its root snapshot");
			const metadata = (snapshot.state as RootState)._meta?.wingman as
				| (WorkspaceScope & { directory: string })
				| undefined;
			if (
				metadata?.instanceId !== this.scope.instanceId ||
				metadata.workspaceId !== this.scope.workspaceId
			) {
				this.replaced = true;
				throw new Error("Workspace changed. Reload to reconnect.");
			}
			this.directory = metadata.directory;
			this.snapshot(snapshot, root);
			this.status(true);
			for (const [uri, observer] of this.observers)
				if (
					uri !== "ahp-root://" &&
					observer.listeners.size &&
					observer.generation !== generation
				)
					void this.attach(uri, observer, generation);
		} catch (error) {
			await client?.shutdown();
			if (generation === this.generation) {
				this.status(false);
				this.schedule();
			}
			throw error;
		}
	}
	private schedule() {
		if (this.stopped || this.replaced) return;
		clearTimeout(this.timer);
		this.timer = setTimeout(() => void this.ready().catch(() => {}), 1000);
	}
	private observe(uri: string): Observer {
		let observer = this.observers.get(uri);
		if (!observer) {
			observer = { listeners: new Set(), seq: -1, pending: [], generation: 0 };
			this.observers.set(uri, observer);
		}
		return observer;
	}
	watch(uri: string, listener: (update: ChannelUpdate) => void): () => void {
		const observer = this.observe(uri);
		observer.listeners.add(listener);
		if (
			this.connected &&
			observer.state &&
			observer.generation === this.generation
		)
			listener({
				state: observer.state,
				snapshot: true,
				connection: this.generation,
			});
		else if (this.connected && observer.generation !== this.generation)
			void this.attach(uri, observer, this.generation);
		let released = false;
		return () => {
			if (released) return;
			released = true;
			observer.listeners.delete(listener);
			if (
				!observer.listeners.size &&
				uri !== "ahp-root://" &&
				this.observers.get(uri) === observer
			) {
				this.observers.delete(uri);
				void this.client?.unsubscribe(uri);
			}
		};
	}
	private async attach(uri: string, observer: Observer, generation: number) {
		observer.generation = generation;
		observer.state = undefined;
		observer.pending = [];
		try {
			const result = await this.client!.request("subscribe", { channel: uri });
			if (
				generation !== this.generation ||
				this.observers.get(uri) !== observer
			)
				return;
			if (!result.snapshot)
				throw new Error("Host did not provide a channel snapshot");
			this.snapshot(result.snapshot, observer);
		} catch (error) {
			if (generation === this.generation && this.connected) {
				observer.generation = 0;
				this.channelErrors.get(uri)?.forEach((fn) => fn(error as Error));
			}
		}
	}
	private channelErrors = new Map<string, Set<(error: Error) => void>>();
	async read<T extends ChannelState>(uri: string): Promise<T> {
		await this.ready();
		return new Promise<T>((resolve, reject) => {
			let release = () => {};
			const errors = this.channelErrors.get(uri) ?? new Set();
			this.channelErrors.set(uri, errors);
			const finish = (error?: Error, state?: ChannelState) => {
				clearTimeout(timer);
				errors.delete(onError);
				if (!errors.size) this.channelErrors.delete(uri);
				queueMicrotask(() => release());
				if (error) reject(error);
				else resolve(state as T);
			};
			const onError = (error: Error) => finish(error);
			errors.add(onError);
			const timer = setTimeout(
				() =>
					finish(
						new Error("Waiting for channel synchronization. Please retry."),
					),
				30000,
			);
			release = this.watch(uri, (update) => finish(undefined, update.state));
		});
	}
	private snapshot(snapshot: Snapshot, observer: Observer) {
		observer.state = snapshot.state as ChannelState;
		observer.seq = snapshot.fromSeq;
		this.changed(snapshot.resource, observer, true);
		const pending = observer.pending;
		observer.pending = [];
		for (const action of pending) this.reduce(action, observer);
	}
	private action(envelope: ActionEnvelope) {
		if (envelope.origin?.clientId === this.clientId) {
			for (const [id, pending] of this.actions)
				if (pending.seq === envelope.origin.clientSeq) {
					clearTimeout(pending.timer);
					if (envelope.rejectionReason)
						pending.reject?.(new Error(envelope.rejectionReason));
					else pending.resolve?.();
					this.actions.delete(id);
					break;
				}
		}
		if (envelope.rejectionReason) return;
		const observer = this.observers.get(envelope.channel);
		if (!observer || observer.generation !== this.generation) return;
		if (!observer.state) {
			if (observer.pending.length >= 4096) {
				void this.client?.shutdown();
				return;
			}
			observer.pending.push(envelope);
		} else this.reduce(envelope, observer);
	}
	private reduce(envelope: ActionEnvelope, observer: Observer) {
		if (envelope.serverSeq <= observer.seq || !observer.state) return;
		const { channel, action } = envelope;
		if (channel === "ahp-root://")
			observer.state = rootReducer(
				observer.state as RootState,
				action as RootAction,
			);
		else if (channel.startsWith("ahp-chat:"))
			observer.state = chatReducer(
				observer.state as ChatState,
				action as ChatAction,
			);
		else if (channel.startsWith("ahp-session:"))
			observer.state = sessionReducer(
				observer.state as SessionState,
				action as SessionAction,
			);
		else if (channel.startsWith("ahp-changeset:"))
			observer.state = changesetReducer(
				observer.state as ChangesetState,
				action as ChangesetAction,
			);
		else if (channel.startsWith("ahp-terminal:"))
			observer.state = terminalReducer(
				observer.state as TerminalState,
				action as TerminalAction,
			);
		observer.seq = envelope.serverSeq;
		this.changed(channel, observer, false, envelope);
	}
	private changed(
		uri: string,
		observer: Observer,
		snapshot: boolean,
		action?: ActionEnvelope,
	) {
		if (uri === "ahp-root://") {
			this.root = observer.state as RootState;
			if (snapshot || action?.action.type === "root/terminalsChanged")
				this.emit({ type: "terminals_changed" });
		}
		if (
			!snapshot &&
			uri.startsWith("ahp-changeset:") &&
			[
				"changeset/fileSet",
				"changeset/fileRemoved",
				"changeset/statusChanged",
			].includes(action?.action.type ?? "")
		)
			this.emit({
				type:
					uri.endsWith("/staged") || uri.endsWith("/unstaged")
						? "git_index_changed"
						: "diffs_changed",
			});
		for (const fn of observer.listeners)
			fn({
				state: observer.state!,
				action,
				snapshot,
				connection: this.generation,
			});
	}
	async request<M extends keyof CommandMap>(
		method: M,
		params: CommandMap[M]["params"],
	): Promise<CommandMap[M]["result"]> {
		await this.ready();
		return this.client!.request(method, params);
	}
	async extension<T = void>(
		method: string,
		params: Record<string, unknown>,
	): Promise<T> {
		await this.ready();
		// The official request helper accepts extension methods on the wire;
		// its generated CommandMap intentionally contains standard methods only.
		return (
			this.client!.request as (method: string, params: unknown) => Promise<T>
		)(method, params);
	}
	async dispatch(
		channel: string,
		action: StateAction,
		id: string = crypto.randomUUID(),
	): Promise<void> {
		await this.ready();
		let pending = this.actions.get(id);
		if (pending?.resolve) throw new Error("This action is already pending");
		if (!pending) {
			if (this.actions.size >= 256)
				throw new Error(
					"Too many unconfirmed actions. Reconnect before retrying.",
				);
			pending = { channel, action, seq: ++this.sequence };
			this.actions.set(id, pending);
		}
		const current = pending;
		return new Promise<void>((resolve, reject) => {
			current.resolve = resolve;
			current.reject = reject;
			current.timer = setTimeout(() => {
				current.resolve = undefined;
				current.reject = undefined;
				reject(new Error("Action delivery unconfirmed. Please retry."));
			}, 30000);
			try {
				this.client!.dispatch(current.channel, current.action, current.seq);
			} catch (error) {
				clearTimeout(current.timer);
				current.resolve = undefined;
				current.reject = undefined;
				reject(error);
			}
		});
	}
}
