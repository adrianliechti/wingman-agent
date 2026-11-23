export interface EditorRequestOptions {
	/** Handles LSP workspace/applyEdit requests issued while this request runs. */
	applyEdit?: (params: any) => Promise<boolean>;
	/** Milliseconds before the request is cancelled; 0 waits indefinitely. */
	timeout?: number;
}

/** The language gateway's socket lifecycle and JSON-RPC envelope. */
export class EditorConnection {
	private socket?: WebSocket;
	private connecting?: Promise<void>;
	private sequence = 0;
	private generation = 0;
	private pending = new Map<
		number,
		{ resolve(value: any): void; reject(error: Error): void; cleanup(): void }
	>();
	private handlers = new Map<number, (params: any) => Promise<boolean>>();
	onReady?: (result: any) => void;
	readonly instance: string;
	readonly initialize: object;
	constructor(instance: string, initialize: object) {
		this.instance = instance;
		this.initialize = initialize;
	}
	close() {
		++this.generation;
		const socket = this.socket;
		this.socket = undefined;
		this.connecting = undefined;
		socket?.close();
		this.rejectPending(new Error("LSP connection closed"));
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
		const url = new URL("/lsp", location.href);
		url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
		url.searchParams.set("instance", this.instance);
		const socket = new WebSocket(url);
		this.socket = socket;
		const opening = new Promise<void>((resolve, reject) => {
			const timer = setTimeout(() => {
				reject(new Error("lsp connection timed out"));
				socket.close();
			}, 10000);
			socket.onopen = () => {
				clearTimeout(timer);
				resolve();
			};
			socket.onerror = () => {
				clearTimeout(timer);
				reject(new Error("lsp connection failed"));
			};
			socket.onclose = () => {
				clearTimeout(timer);
				reject(new Error("lsp connection closed"));
				if (generation !== this.generation) return;
				this.socket = undefined;
				this.connecting = undefined;
				this.rejectPending(
					new Error("lsp connection lost; refresh before retrying"),
				);
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
				this.notify("initialized", {});
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
		this.send({ jsonrpc: "2.0", method, params });
	}
	private send(value: unknown) {
		if (this.socket?.readyState !== WebSocket.OPEN)
			throw new Error("lsp connection is unavailable");
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
				if (this.socket?.readyState === WebSocket.OPEN)
					this.notify("$/cancelRequest", { id });
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
				this.send({ jsonrpc: "2.0", id, method, params });
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
		if (message.method) {
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
			}
			return;
		}
		const pending = this.pending.get(message.id);
		if (!pending) return;
		this.pending.delete(message.id);
		pending.cleanup();
		if (message.error)
			pending.reject(
				new Error(message.error.message ?? "Protocol request failed"),
			);
		else pending.resolve(message.result);
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
