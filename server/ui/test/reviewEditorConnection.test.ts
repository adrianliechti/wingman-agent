import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { EditorConnection } from "../src/state/editorConnection.ts";
import { LSPClient } from "../src/state/lspClient.ts";

class ReviewSocket {
	static OPEN = 1;
	static instances: ReviewSocket[] = [];
	readyState = 0;
	onopen?: () => void;
	onclose?: () => void;
	onerror?: () => void;
	onmessage?: (event: { data: string }) => void;
	sent: any[] = [];
	constructor() {
		ReviewSocket.instances.push(this);
		queueMicrotask(() => {
			this.readyState = 1;
			this.onopen?.();
		});
	}
	send(wire: string) {
		const message = JSON.parse(wire);
		this.sent.push(message);
		if (message.method === "initialize")
			queueMicrotask(() =>
				this.reply(message, {
					capabilities: {
						experimental: { wingman: { workspaceURI: "file:///work/" } },
					},
				}),
			);
	}
	receive(message: unknown) {
		this.onmessage?.({
			data: typeof message === "string" ? message : JSON.stringify(message),
		});
	}
	reply(request: any, result: unknown) {
		this.receive({ jsonrpc: "2.0", id: request.id, result });
	}
	close() {
		if (this.readyState === 3) return;
		this.readyState = 3;
		this.onclose?.();
	}
}
const reviewOriginal = globalThis.WebSocket;
beforeEach(() => {
	ReviewSocket.instances = [];
	Object.assign(globalThis, {
		WebSocket: ReviewSocket,
		location: { href: "http://wingman.test/" },
	});
});
afterEach(() => {
	for (const socket of ReviewSocket.instances) socket.close();
	Object.assign(globalThis, { WebSocket: reviewOriginal });
});
async function reviewUntil(predicate: () => boolean) {
	for (let i = 0; i < 100; i++) {
		if (predicate()) return;
		await new Promise((resolve) => setImmediate(resolve));
	}
	assert.fail("condition did not become true");
}

test("LSP edits made while reconnecting reopen the latest buffer before newer changes", async () => {
	const client = new LSPClient("one");
	await client.sync("open", "a.ts", "v1");
	await client.sync("change", "a.ts", "v2");
	ReviewSocket.instances[0].close();
	const edits = [
		client.sync("change", "a.ts", "v3"),
		client.sync("change", "a.ts", "v3"),
		client.sync("save", "a.ts", "v4"),
	];
	await Promise.all(edits);
	const second = ReviewSocket.instances[1];
	const documents = second.sent
		.filter((m) => m.method?.startsWith("textDocument/"))
		.map((m) => [
			m.method,
			m.params.textDocument.version,
			m.params.contentChanges?.[0].text ??
				m.params.textDocument.text ??
				m.params.text,
		]);
	assert.deepEqual(documents, [
		["textDocument/didOpen", 2, "v2"],
		["textDocument/didChange", 3, "v3"],
		["textDocument/didChange", 4, "v4"],
		["textDocument/didSave", undefined, "v4"],
	]);
	await client.sync("close", "a.ts");
	await client.sync("close", "a.ts");
	assert.equal(
		second.sent.filter((m) => m.method === "textDocument/didClose").length,
		1,
	);
	client.close();
});

test("LSP refuses unknown server requests and edits for requests it did not make", async () => {
	const connection = new EditorConnection("one", {});
	await connection.ready();
	const socket = ReviewSocket.instances[0];
	socket.receive({
		jsonrpc: "2.0",
		id: 7,
		method: "window/showDocument",
		params: {},
	});
	socket.receive({
		jsonrpc: "2.0",
		id: 8,
		method: "workspace/applyEdit",
		params: { edit: {}, _wingmanRequestId: 999 },
	});
	await reviewUntil(() => socket.sent.filter((m) => m.id >= 7).length === 2);
	assert.deepEqual(
		socket.sent
			.filter((m) => m.id >= 7)
			.map((m) => [m.id, m.error?.code ?? m.result]),
		[
			[7, -32601],
			[8, -32601],
		],
	);
	connection.close();
});

test("LSP timeouts cancel on the server, ignore late results and keep the socket usable", async (t) => {
	const connection = new EditorConnection("one", {});
	await connection.ready();
	const socket = ReviewSocket.instances[0];
	t.mock.timers.enable({ apis: ["setTimeout"] });
	const slow = connection.request("textDocument/hover", {}, undefined, {
		timeout: 50,
	});
	const rejected = assert.rejects(slow, { name: "TimeoutError" });
	await reviewUntil(() =>
		socket.sent.some((m) => m.method === "textDocument/hover"),
	);
	t.mock.timers.tick(50);
	await rejected;
	const hover = socket.sent.find((m) => m.method === "textDocument/hover");
	assert.deepEqual(socket.sent.at(-1), {
		jsonrpc: "2.0",
		method: "$/cancelRequest",
		params: { id: hover.id },
	});
	socket.reply(hover, { contents: "late" });
	const next = connection.request("textDocument/definition", {});
	await reviewUntil(
		() => socket.sent.at(-1).method === "textDocument/definition",
	);
	socket.reply(socket.sent.at(-1), []);
	assert.deepEqual(await next, []);
	connection.close();
});
