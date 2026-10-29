import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { EditorConnection } from "../src/state/editorConnection.ts";
import { LSPClient } from "../src/state/lspClient.ts";
import { DAPClient } from "../src/state/dapClient.ts";

class Socket {
	static OPEN = 1;
	static instances: Socket[] = [];
	readyState = 0;
	onopen?: () => void;
	onclose?: () => void;
	onerror?: () => void;
	onmessage?: (event: { data: string }) => void;
	sent: any[] = [];
	url: URL;
	constructor(url: URL) {
		this.url = url;
		Socket.instances.push(this);
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
					_wingman: { workspacePath: "/workspace" },
					capabilities: {
						experimental: { wingman: { workspaceURI: "file:///workspace" } },
					},
				}),
			);
	}
	receive(message: any) {
		this.onmessage?.({ data: JSON.stringify(message) });
	}
	reply(request: any, result: any) {
		this.receive(
			request.jsonrpc
				? { jsonrpc: "2.0", id: request.id, result }
				: {
						seq: 100,
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
const original = globalThis.WebSocket;
beforeEach(() => {
	Socket.instances = [];
	Object.assign(globalThis, {
		WebSocket: Socket,
		location: { href: "https://wingman.example/" },
	});
});
afterEach(() => {
	for (const socket of Socket.instances) socket.close();
	Object.assign(globalThis, { WebSocket: original });
});
async function until(predicate: () => boolean) {
	for (let i = 0; i < 100; i++) {
		if (predicate()) return;
		await new Promise((resolve) => setImmediate(resolve));
	}
	assert.fail("condition did not become true");
}

test("DAP rejects launch promptly when configuration fails", async () => {
	const client = new DAPClient("one");
	const launch = client.launch({ request: "launch" } as any);
	const rejected = assert.rejects(launch, /configuration rejected/);
	await until(() => Socket.instances[0]?.sent.at(-1)?.command === "launch");
	const socket = Socket.instances[0];
	const request = socket.sent.at(-1);
	socket.receive({ seq: 2, type: "event", event: "initialized", body: {} });
	await until(() => socket.sent.at(-1).command === "configurationDone");
	const config = socket.sent.at(-1);
	socket.receive({
		seq: 3,
		type: "response",
		request_seq: config.seq,
		command: config.command,
		success: false,
		message: "configuration rejected",
	});
	await rejected;
	assert.ok(
		socket.sent.some(
			(message) =>
				message.command === "cancel" &&
				message.arguments.requestId === request.seq,
		),
	);
	client.close();
});

test("LSP sends ordered document changes, UTF-16 positions, and restores buffers on reconnect", async () => {
	const client = new LSPClient("instance-one");
	await client.sync("open", "src/a b.ts", "const x = '🌍';");
	await client.sync("change", "src/a b.ts", "const x = '🌍'; x.");
	const request = client.feature("textDocument/completion", {
		path: "src/a b.ts",
		content: "const x = '🌍'; x.",
		line: 1,
		column: 19,
	});
	const socket = Socket.instances[0];
	await until(() =>
		socket.sent.some((m) => m.method === "textDocument/completion"),
	);
	assert.equal(
		socket.url.href,
		"wss://wingman.example/lsp?instance=instance-one",
	);
	const open = socket.sent.find((m) => m.method === "textDocument/didOpen");
	assert.equal(open.params.textDocument.uri, "file:///workspace/src/a%20b.ts");
	assert.equal(open.params.textDocument.version, 1);
	const change = socket.sent.find((m) => m.method === "textDocument/didChange");
	assert.equal(change.params.textDocument.version, 2);
	const completion = socket.sent.at(-1);
	assert.deepEqual(completion.params.position, { line: 0, character: 18 });
	assert.equal(completion.params.content, undefined);
	socket.reply(completion, { isIncomplete: false, items: [] });
	await request;
	socket.close();
	await client.connection.ready();
	const restored = Socket.instances[1].sent.find(
		(m) => m.method === "textDocument/didOpen",
	);
	assert.equal(restored.params.textDocument.text, "const x = '🌍'; x.");
	assert.equal(restored.params.textDocument.version, 2);
	client.close();
});

test("LSP cancels requests, correlates out-of-order results and answers workspace/applyEdit", async () => {
	const client = new EditorConnection("lsp", "one", {});
	await client.ready();
	const socket = Socket.instances[0];
	const cancel = new AbortController();
	const a = client.request("textDocument/hover", {}, cancel.signal);
	const rejected = assert.rejects(a, { name: "AbortError" });
	const b = client.request("textDocument/definition", {});
	await until(() =>
		socket.sent.some((m) => m.method === "textDocument/definition"),
	);
	const definition = socket.sent.find(
		(message) => message.method === "textDocument/definition",
	);
	socket.reply(definition, [{ uri: "file:///workspace/a.ts" }]);
	assert.equal(((await b) as any[]).length, 1);
	cancel.abort();
	await rejected;
	assert.equal(socket.sent.at(-1).method, "$/cancelRequest");
	let edit: unknown;
	const command = client.request(
		"workspace/executeCommand",
		{ command: "fix" },
		undefined,
		{
			applyEdit: async (params) => {
				edit = params.edit;
				return true;
			},
		},
	);
	await until(() => socket.sent.at(-1).method === "workspace/executeCommand");
	const request = socket.sent.at(-1);
	socket.receive({
		jsonrpc: "2.0",
		id: "apply:1",
		method: "workspace/applyEdit",
		params: { edit: { changes: {} }, _wingmanRequestId: request.id },
	});
	await until(() => socket.sent.at(-1).id === "apply:1");
	assert.deepEqual(edit, { changes: {} });
	assert.deepEqual(socket.sent.at(-1).result, { applied: true });
	socket.reply(request, null);
	await command;
	client.close();
});

test("DAP uses request_seq and events, and never replays an uncertain launch", async () => {
	const client = new EditorConnection("dap", "one", { adapterID: "wingman" });
	await client.ready();
	const socket = Socket.instances[0];
	const events: string[] = [];
	client.onEvent((event) => events.push(event));
	const launch = client.request("launch", {
		_wingmanPlan: { request: "launch" },
	});
	const rejected = assert.rejects(launch, /connection lost/);
	await until(() => socket.sent.at(-1).command === "launch");
	const request = socket.sent.at(-1);
	assert.equal(request.type, "request");
	assert.equal(request.jsonrpc, undefined);
	assert.equal(typeof request.seq, "number");
	socket.receive({ seq: 2, type: "event", event: "initialized" });
	assert.deepEqual(events, ["initialized"]);
	socket.close();
	await rejected;
	await client.ready();
	assert.deepEqual(
		Socket.instances[1].sent.map((m) => m.command),
		["initialize"],
	);
	const threads = client.request("threads", {});
	await until(() => Socket.instances[1].sent.at(-1).command === "threads");
	const second = Socket.instances[1];
	second.reply(second.sent.at(-1), { threads: [] });
	assert.deepEqual(await threads, { threads: [] });
	client.close();
});
