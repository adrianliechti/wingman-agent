import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { EditorConnection } from "../src/state/editorConnection.ts";
import { LSPClient } from "../src/state/lspClient.ts";
import { DAPClient } from "../src/state/dapClient.ts";

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
		if ((message.method ?? message.command) === "initialize")
			queueMicrotask(() =>
				this.reply(message, {
					_wingman: { workspacePath: "C:\\Work\\" },
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
		this.receive(
			request.jsonrpc
				? { jsonrpc: "2.0", id: request.id, result }
				: {
						seq: 0,
						type: "response",
						request_seq: request.seq,
						command: request.command,
						success: true,
						body: result,
					},
		);
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
	const connection = new EditorConnection("lsp", "one", {});
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
	socket.receive({
		jsonrpc: "2.0",
		id: 9,
		method: "workspace/diagnostic/refresh",
	});
	await reviewUntil(() => socket.sent.filter((m) => m.id >= 7).length === 3);
	assert.deepEqual(
		socket.sent
			.filter((m) => m.id >= 7)
			.map((m) => [m.id, m.error?.code ?? m.result]),
		[
			[7, -32601],
			[8, -32601],
			[9, null],
		],
	);
	connection.close();
});

test("LSP timeouts cancel on the server, ignore late results and keep the socket usable", async (t) => {
	const connection = new EditorConnection("lsp", "one", {});
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

test("DAP output joins snapshots and overlapping events without gaps or repeats", async () => {
	const client = new DAPClient("one");
	const outputs: string[] = [];
	client.subscribe((event) => {
		if (event === "output") outputs.push(event);
	});
	const status = client.status();
	await reviewUntil(
		() => ReviewSocket.instances[0]?.sent.at(-1)?.command === "wingman/status",
	);
	const socket = ReviewSocket.instances[0];
	const request = socket.sent.at(-1);
	const session = { session_id: "s1", state: "running", state_version: 1 };
	// Events raced ahead of the snapshot response, which covers the first of them.
	socket.receive({
		seq: 1,
		type: "event",
		event: "output",
		body: { output: "abc", _wingmanOffset: 0, _wingmanSessionId: "s1" },
	});
	socket.receive({
		seq: 2,
		type: "event",
		event: "output",
		body: { output: "def", _wingmanOffset: 3, _wingmanSessionId: "s1" },
	});
	socket.reply(request, {
		session,
		output: "abc",
		_wingmanOutputOffset: 3,
		breakpoints: [],
		resolved: [],
	});
	assert.equal((await status).output, "abcdef");
	socket.receive({
		seq: 3,
		type: "event",
		event: "output",
		body: { output: "efgh", _wingmanOffset: 4, _wingmanSessionId: "s1" },
	});
	socket.receive({
		seq: 4,
		type: "event",
		event: "output",
		body: { output: "gh", _wingmanOffset: 6, _wingmanSessionId: "s1" },
	});
	assert.equal((await client.inspection(false)).output, "abcdefgh");
	socket.receive({
		seq: 5,
		type: "event",
		event: "output",
		body: { output: "new", _wingmanOffset: 0, _wingmanSessionId: "s2" },
	});
	assert.equal((await client.inspection(false)).output, "new");
	assert.equal(outputs.length, 5);
	client.close();
});

test("DAP maps Windows frame paths into the workspace case-insensitively", async () => {
	const client = new DAPClient("one");
	const state = client.state("src/main.go");
	await reviewUntil(
		() => ReviewSocket.instances[0]?.sent.at(-1)?.command === "wingman/status",
	);
	const socket = ReviewSocket.instances[0];
	const request = socket.sent.at(-1);
	assert.deepEqual(request.arguments.source, { path: "C:/Work/src/main.go" });
	const session = {
		session_id: "s1",
		state: "stopped",
		state_version: 2,
		stop: { thread_id: 4 },
	};
	socket.reply(request, {
		session,
		output: "",
		_wingmanOutputOffset: 0,
		breakpoints: [],
		resolved: [],
	});
	await reviewUntil(() => socket.sent.at(-1).command === "stackTrace");
	const stack = socket.sent.at(-1);
	assert.deepEqual(
		[
			stack.arguments.threadId,
			stack.arguments._wingmanSessionId,
			stack.arguments._wingmanStateVersion,
		],
		[4, "s1", 2],
	);
	socket.reply(stack, {
		stackFrames: [
			{
				id: 1,
				name: "main",
				line: 3,
				column: 1,
				source: { path: "c:\\work\\src\\main.go" },
			},
		],
	});
	assert.equal((await state).frame?.source?.path, "src/main.go");
	client.close();
});
