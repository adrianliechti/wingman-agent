export interface EditorRequestOptions {
	/** Handles LSP workspace/applyEdit requests issued while this request runs. */
	applyEdit?: (params: any) => Promise<boolean>;
	/** Milliseconds before the request is cancelled; 0 waits indefinitely. */
	timeout?: number;
}

/** Shared socket lifecycle only. LSP is JSON-RPC; DAP keeps its own envelope. */
export class EditorConnection {
	private socket?: WebSocket;
	private connecting?: Promise<void>;
	private sequence = 0;
	private generation = 0;
	private pending = new Map<
		number,
		{ resolve(value: any): void; reject(error: Error): void; cleanup(): void }
	>();
	private listeners = new Set<(method: string, params: any) => void>();
	private handlers = new Map<number, (params: any) => Promise<boolean>>();
	onReady?: (result: any) => void;
	readonly mode: "lsp" | "dap";
	readonly instance: string;
	readonly initialize: object;
	constructor(mode: "lsp" | "dap", instance: string, initialize: object) {
		this.mode = mode;
		this.instance = instance;
		this.initialize = initialize;
	}
	onEvent(listener: (method: string, params: any) => void) {
		this.listeners.add(listener);
		return () => {
			this.listeners.delete(listener);
		};
	}
	close() {
		++this.generation;
		const socket = this.socket;
		this.socket = undefined;
		this.connecting = undefined;
		socket?.close();
		this.rejectPending(
			new Error(`${this.mode.toUpperCase()} connection closed`),
		);
	}
	private rejectPending(error: Error) {
		for (const p of this.pending.values()) {
			p.cleanup();
			p.reject(error);
		}
		this.pending.clear();
	}
	private async connect() {
		if (this.connecting) return this.connecting;
		const generation = ++this.generation;
		const url = new URL(`/${this.mode}`, location.href);
		url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
		url.searchParams.set("instance", this.instance);
		const socket = new WebSocket(url);
		this.socket = socket;
		const opening = new Promise<void>((resolve, reject) => {
			const timer = setTimeout(() => {
				reject(new Error(`${this.mode} connection timed out`));
				socket.close();
			}, 10000);
			socket.onopen = () => {
				clearTimeout(timer);
				resolve();
			};
			socket.onerror = () => {
				clearTimeout(timer);
				reject(new Error(`${this.mode} connection failed`));
			};
			socket.onclose = () => {
				clearTimeout(timer);
				reject(new Error(`${this.mode} connection closed`));
				if (generation !== this.generation) return;
				this.socket = undefined;
				this.connecting = undefined;
				this.rejectPending(
					new Error(`${this.mode} connection lost; refresh before retrying`),
				);
				for (const listener of this.listeners) listener("disconnected", {});
			};
			socket.onmessage = (event) => {
				if (generation === this.generation)
					void this.receive(String(event.data));
			};
		});
		const connecting = opening
			.then(async () => {
				const result = await this.sendRequest("initialize", this.initialize);
				if (generation !== this.generation)
					throw new Error("Connection was replaced");
				if (this.mode === "lsp") this.notify("initialized", {});
				this.onReady?.(result);
			})
			.catch((error) => {
				if (generation === this.generation) {
					this.connecting = undefined;
					socket.close();
				}
				throw error;
			});
		this.connecting = connecting;
		return connecting;
	}
	async ready() {
		await this.connect();
	}
	async request<T>(
		method: string,
		params: unknown,
		signal?: AbortSignal,
		options: EditorRequestOptions = {},
	): Promise<T> {
		signal?.throwIfAborted();
		await abortable(this.connect(), signal);
		signal?.throwIfAborted();
		return this.sendRequest(method, params, signal, options);
	}
	notify(method: string, params: unknown) {
		if (this.mode !== "lsp")
			throw new Error("DAP does not have JSON-RPC notifications");
		this.send({ jsonrpc: "2.0", method, params });
	}
	private send(value: unknown) {
		if (this.socket?.readyState !== WebSocket.OPEN)
			throw new Error(`${this.mode} connection is unavailable`);
		this.socket.send(JSON.stringify(value));
	}
	private sendRequest<T>(
		method: string,
		params: unknown,
		signal?: AbortSignal,
		{ applyEdit, timeout = 120000 }: EditorRequestOptions = {},
	): Promise<T> {
		const id = ++this.sequence;
		return new Promise<T>((resolve, reject) => {
			const cancel = () => {
				if (!this.pending.delete(id)) return;
				cleanup();
				reject(
					signal?.reason ??
						new DOMException("Request timed out", "TimeoutError"),
				);
				if (this.socket?.readyState === WebSocket.OPEN) {
					if (this.mode === "lsp") this.notify("$/cancelRequest", { id });
					else
						this.send({
							seq: ++this.sequence,
							type: "request",
							command: "cancel",
							arguments: { requestId: id },
						});
				}
			};
			const timer = timeout > 0 ? setTimeout(cancel, timeout) : undefined;
			const cleanup = () => {
				clearTimeout(timer);
				signal?.removeEventListener("abort", cancel);
				this.handlers.delete(id);
			};
			this.pending.set(id, { resolve, reject, cleanup });
			if (applyEdit) this.handlers.set(id, applyEdit);
			signal?.addEventListener("abort", cancel, { once: true });
			try {
				this.send(
					this.mode === "lsp"
						? { jsonrpc: "2.0", id, method, params }
						: { seq: id, type: "request", command: method, arguments: params },
				);
			} catch (error) {
				this.pending.delete(id);
				cleanup();
				reject(error);
			}
		});
	}
	private async receive(wire: string) {
		const socket = this.socket;
		let message: any;
		try {
			message = JSON.parse(wire);
		} catch {
			this.close();
			return;
		}
		if (this.mode === "lsp" && message.method) {
			if (message.id !== undefined) {
				const handler =
					message.method === "workspace/applyEdit"
						? this.handlers.get(message.params?._wingmanRequestId)
						: undefined;
				try {
					if (!handler)
						this.send({
							jsonrpc: "2.0",
							id: message.id,
							error: { code: -32601, message: "Client request is unsupported" },
						});
					else {
						const applied = await handler(message.params);
						if (socket === this.socket)
							this.send({
								jsonrpc: "2.0",
								id: message.id,
								result: { applied },
							});
					}
				} catch (error) {
					if (
						socket === this.socket &&
						this.socket?.readyState === WebSocket.OPEN
					)
						this.send({
							jsonrpc: "2.0",
							id: message.id,
							result: { applied: false, failureReason: String(error) },
						});
				}
			} else
				for (const listener of this.listeners)
					listener(message.method, message.params);
			return;
		}
		if (this.mode === "dap" && message.type === "event") {
			for (const listener of this.listeners)
				listener(message.event, message.body);
			return;
		}
		const id = this.mode === "lsp" ? message.id : message.request_seq;
		const pending = this.pending.get(id);
		if (!pending) return;
		this.pending.delete(id);
		pending.cleanup();
		if (message.error || message.success === false)
			pending.reject(
				new Error(
					message.error?.message ??
						message.body?.error?.format ??
						message.message ??
						"Protocol request failed",
				),
			);
		else pending.resolve(this.mode === "lsp" ? message.result : message.body);
	}
}

async function abortable<T>(
	promise: Promise<T>,
	signal?: AbortSignal,
): Promise<T> {
	if (!signal) return promise;
	let abort = () => {};
	try {
		return await Promise.race([
			promise,
			new Promise<T>((_resolve, reject) => {
				abort = () => reject(signal.reason);
				signal.addEventListener("abort", abort, { once: true });
				if (signal.aborted) abort();
			}),
		]);
	} finally {
		signal.removeEventListener("abort", abort);
	}
}
