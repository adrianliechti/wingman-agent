import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { setImmediate } from "node:timers/promises";
import {
	InMemoryTransport,
	type AhpTransport,
} from "@microsoft/agent-host-protocol/client";
import { AHPConnection } from "../src/state/ahpConnection.ts";
import { createWorkspaceHTTP } from "../src/api/http.ts";

const scope = { workspaceId: "workspace", instanceId: "instance" };
async function fixture(
	t: TestContext,
	handler: (message: any, server: AhpTransport) => Promise<void> | void,
) {
	const requests: any[] = [],
		peers: AhpTransport[] = [];
	const ahp = new AHPConnection(scope, async () => {
		const [client, server] = InMemoryTransport.pair();
		peers.push(server);
		void (async () => {
			for (;;) {
				const frame = await server.recv();
				if (!frame) return;
				const message =
					frame.kind === "text"
						? JSON.parse(frame.text)
						: frame.kind === "parsed"
							? frame.message
							: {};
				requests.push(message);
				if (message.method === "initialize")
					await server.send(
						JSON.stringify({
							jsonrpc: "2.0",
							id: message.id,
							result: {
								protocolVersion: "1.0.0",
								serverSeq: 0,
								snapshots: [
									{
										resource: "ahp-root://",
										fromSeq: 0,
										state: {
											agents: [],
											_meta: {
												"wingman.workspace": scope,
											},
										},
									},
								],
							},
						}),
					);
				else await handler(message, server);
			}
		})();
		return client;
	});
	t.after(() => ahp.stop());
	ahp.start();
	await ahp.ready();
	return { ahp, requests, peers };
}
function reply(server: AhpTransport, id: number, state: unknown, fromSeq = 0) {
	return server.send(
		JSON.stringify({
			jsonrpc: "2.0",
			id,
			result: { snapshot: { resource: "ahp-terminal:/test", fromSeq, state } },
		}),
	);
}
const terminal = {
	title: "test",
	lifecycle: { status: "running" },
	claim: { kind: "client", clientId: "host" },
	content: [],
};

test("subscription round-trip buffers actions and ignores duplicate receipts", async (t) => {
	const { ahp } = await fixture(t, async (m, server) => {
		if (m.method !== "subscribe") return;
		await reply(server, m.id, terminal, 4);
		for (const seq of [5, 5, 6])
			await server.send(
				JSON.stringify({
					jsonrpc: "2.0",
					method: "action",
					params: {
						channel: m.params.channel,
						serverSeq: seq,
						action: { type: "terminal/data", data: String(seq) },
					},
				}),
			);
	});
	let state: any;
	const release = ahp.watch("ahp-terminal:/test", (event) => {
		state = event.state;
	});
	await setImmediate();
	await setImmediate();
	assert.equal(state.content.map((p: any) => p.value).join(""), "56");
	release();
});
test("observer release is idempotent and final release unsubscribes exactly once", async (t) => {
	const { ahp, requests } = await fixture(t, (m, s) =>
		m.method === "subscribe" ? reply(s, m.id, terminal) : undefined,
	);
	const a = ahp.watch("ahp-terminal:/test", () => {}),
		b = ahp.watch("ahp-terminal:/test", () => {});
	await setImmediate();
	a();
	a();
	await setImmediate();
	assert.equal(requests.filter((m) => m.method === "unsubscribe").length, 0);
	b();
	await setImmediate();
	assert.equal(requests.filter((m) => m.method === "unsubscribe").length, 1);
	assert.equal(requests.filter((m) => m.method === "subscribe").length, 1);
});
test("stopping promptly rejects a read waiting for its snapshot", async (t) => {
	const { ahp } = await fixture(t, () => {});
	const read = ahp.read("ahp-terminal:/test");
	await setImmediate();
	const rejected = assert.rejects(read, /stopped/);
	ahp.stop();
	await rejected;
});
test("unconfirmed action retry retains the original sequence and data", async (t) => {
	let attempts = 0;
	const { ahp, peers, requests } = await fixture(t, async (m, s) => {
		if (m.method !== "dispatchAction") return;
		if (++attempts === 1) {
			await s.close();
			return;
		}
		await s.send(
			JSON.stringify({
				jsonrpc: "2.0",
				method: "action",
				params: {
					...m.params,
					serverSeq: 1,
					origin: { clientId: ahp.clientId, clientSeq: m.params.clientSeq },
				},
			}),
		);
	});
	await assert.rejects(
		ahp.dispatch(
			"ahp-terminal:/test",
			{ type: "terminal/input", data: "original" } as any,
			"input",
		),
		/lost/,
	);
	await ahp.ready();
	await ahp.dispatch(
		"ahp-terminal:/test",
		{ type: "terminal/input", data: "changed" } as any,
		"input",
	);
	const sent = requests.filter((m) => m.method === "dispatchAction");
	assert.equal(peers.length, 2);
	assert.deepEqual(sent[0].params, sent[1].params);
});
test("a different host instance is never used for saved commands", async (t) => {
	const [transport, server] = InMemoryTransport.pair();
	const ahp = new AHPConnection(scope, async () => transport);
	t.after(() => ahp.stop());
	void (async () => {
		const frame = await server.recv();
		const request = JSON.parse((frame as any).text);
		await server.send(
			JSON.stringify({
				jsonrpc: "2.0",
				id: request.id,
				result: {
					protocolVersion: "1.0.0",
					serverSeq: 0,
					snapshots: [
						{
							resource: "ahp-root://",
							fromSeq: 0,
							state: {
								agents: [],
								_meta: {
									"wingman.workspace": { ...scope, instanceId: "replacement" },
								},
							},
						},
					],
				},
			}),
		);
	})();
	ahp.start();
	await assert.rejects(ahp.ready(), /Workspace changed/);
	assert.equal(ahp.getReplaced(), true);
});
test("auxiliary HTTP requests retain their instance, body and custom headers", async (t) => {
	let received: Request | undefined;
	t.mock.method(
		globalThis,
		"fetch",
		async (input: RequestInfo | URL, init?: RequestInit) => {
			received = new Request(input, init);
			return Response.json({});
		},
	);
	const http = createWorkspaceHTTP("bound-instance");
	await http.scopedFetch(
		new Request("https://workspace.test/api/example", {
			method: "POST",
			headers: { "Content-Type": "application/json", "X-Custom": "kept" },
			body: '{"value":1}',
		}),
	);
	assert.equal(received!.headers.get("X-Wingman-Instance"), "bound-instance");
	assert.equal(received!.headers.get("X-Custom"), "kept");
	assert.equal(await received!.text(), '{"value":1}');
});
