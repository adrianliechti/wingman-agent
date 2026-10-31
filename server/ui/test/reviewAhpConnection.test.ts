import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { setImmediate } from "node:timers/promises";
import {
	InMemoryTransport,
	type AhpTransport,
} from "@microsoft/agent-host-protocol/client";
import {
	AHPConnection,
	type ChannelUpdate,
} from "../src/state/ahpConnection.ts";

const reviewScope = { workspaceId: "workspace", instanceId: "instance" };
const reviewTerminalURI = "ahp-terminal:/review";
const reviewTerminal = (value = "") => ({
	title: "review",
	lifecycle: { status: "running" },
	claim: { kind: "client", clientId: "host" },
	content: value ? [{ type: "unclassified", value }] : [],
});

type ReviewPeer = {
	server: AhpTransport;
	send(message: unknown): Promise<void>;
	reply(id: number, result: unknown): Promise<void>;
	data(serverSeq: number, data: string, channel?: string): Promise<void>;
};
/** Each dial is a new peer; the handler answers everything but initialize. */
async function reviewConnection(
	t: TestContext,
	handler: (message: any, peer: ReviewPeer) => Promise<void> | void,
	options: { instance?: () => string } = {},
) {
	const requests: any[] = [],
		peers: ReviewPeer[] = [];
	const ahp = new AHPConnection(reviewScope, async () => {
		const [client, server] = InMemoryTransport.pair();
		const peer: ReviewPeer = {
			server,
			send: async (message) => server.send(JSON.stringify(message)),
			reply: (id, result) => peer.send({ jsonrpc: "2.0", id, result }),
			data: (serverSeq, data, channel = reviewTerminalURI) =>
				peer.send({
					jsonrpc: "2.0",
					method: "action",
					params: {
						channel,
						serverSeq,
						action: { type: "terminal/data", data },
					},
				}),
		};
		peers.push(peer);
		void (async () => {
			for (;;) {
				const frame = await server.recv();
				if (!frame) return;
				const message = frame.kind === "text" ? JSON.parse(frame.text) : {};
				requests.push(message);
				if (message.method === "initialize")
					await peer.reply(message.id, {
						protocolVersion: "1.1.0",
						serverSeq: 0,
						defaultDirectory: "file:///workspace",
						snapshots: [
							{
								resource: "ahp-root://",
								fromSeq: 0,
								state: {
									agents: [],
									_meta: {
										"wingman.workspace": {
											...reviewScope,
											instanceId:
												options.instance?.() ?? reviewScope.instanceId,
										},
									},
								},
							},
						],
					});
				else await handler(message, peer);
			}
		})();
		return client;
	});
	t.after(() => ahp.stop());
	ahp.start();
	await ahp.ready();
	const sent = (method: string) => requests.filter((m) => m.method === method);
	return { ahp, peers, requests, sent };
}
async function reviewUntil(predicate: () => boolean, message = "condition") {
	for (let i = 0; i < 200; i++) {
		if (predicate()) return;
		await setImmediate();
	}
	assert.fail(`${message} did not become true`);
}
const reviewText = (state: any) =>
	state.content.map((part: any) => part.value ?? part.output).join("");
function reviewAck(
	peer: ReviewPeer,
	m: any,
	ahp: AHPConnection,
	serverSeq: number,
	rejectionReason?: string,
) {
	return peer.send({
		jsonrpc: "2.0",
		method: "action",
		params: {
			channel: m.params.channel,
			action: m.params.action,
			serverSeq,
			origin: { clientId: ahp.clientId, clientSeq: m.params.clientSeq },
			...(rejectionReason ? { rejectionReason } : {}),
		},
	});
}

test("a reconnect resubscribes and applies only actions newer than the fresh snapshot", async (t) => {
	let connection = 0;
	const { ahp, peers } = await reviewConnection(t, async (m, peer) => {
		if (m.method !== "subscribe") return;
		if (connection++ === 0) {
			await peer.reply(m.id, {
				snapshot: {
					resource: m.params.channel,
					fromSeq: 3,
					state: reviewTerminal("abc"),
				},
			});
			await peer.data(4, "d");
			return;
		}
		// Live actions racing the snapshot are buffered, then filtered by serverSeq.
		await peer.data(7, "X");
		await peer.data(9, "g");
		await peer.reply(m.id, {
			snapshot: {
				resource: m.params.channel,
				fromSeq: 8,
				state: reviewTerminal("abcdef"),
			},
		});
		await peer.data(9, "g");
		await peer.data(10, "h");
	});
	const updates: ChannelUpdate[] = [];
	const release = ahp.watch(reviewTerminalURI, (update) =>
		updates.push(update),
	);
	await reviewUntil(() => updates.length === 2);
	assert.equal(reviewText(updates[1].state), "abcd");
	await peers[0].server.close();
	await reviewUntil(() => !ahp.getConnected(), "disconnect");
	await ahp.ready();
	await reviewUntil(
		() => reviewText(updates.at(-1)!.state) === "abcdefgh",
		"replay",
	);
	const fresh = updates.slice(2);
	assert.deepEqual(
		fresh.map((update) => [update.snapshot, update.action?.serverSeq]),
		[
			[true, undefined],
			[false, 9],
			[false, 10],
		],
	);
	assert.ok(fresh.every((update) => update.connection === fresh[0].connection));
	assert.notEqual(fresh[0].connection, updates[0].connection);
	release();
});

test("own rejected actions reject their dispatch without touching state", async (t) => {
	const { ahp } = await reviewConnection(t, async (m, peer) => {
		if (m.method === "subscribe")
			await peer.reply(m.id, {
				snapshot: {
					resource: m.params.channel,
					fromSeq: 0,
					state: reviewTerminal("x"),
				},
			});
		else if (m.method === "dispatchAction") {
			// Another client's rejection reaches subscribers too and must be ignored.
			await peer.send({
				jsonrpc: "2.0",
				method: "action",
				params: {
					channel: m.params.channel,
					serverSeq: 1,
					action: { type: "terminal/data", data: "other" },
					origin: { clientId: "someone-else", clientSeq: m.params.clientSeq },
					rejectionReason: "not yours",
				},
			});
			await reviewAck(peer, m, ahp, 2, "terminal is read-only");
		}
	});
	let state: any;
	const release = ahp.watch(
		reviewTerminalURI,
		(update) => (state = update.state),
	);
	await reviewUntil(() => state !== undefined);
	await assert.rejects(
		ahp.dispatch(reviewTerminalURI, {
			type: "terminal/data",
			data: "mine",
		} as any),
		/terminal is read-only/,
	);
	assert.equal(reviewText(state), "x");
	release();
});

test("a timed-out action is retried under its sequence and a fresh one gets the next", async (t) => {
	t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
	let attempts = 0;
	const { ahp, sent } = await reviewConnection(t, async (m, peer) => {
		if (m.method === "dispatchAction" && ++attempts > 1)
			await reviewAck(peer, m, ahp, attempts);
	});
	const first = ahp.dispatch(
		reviewTerminalURI,
		{ type: "terminal/input", data: "a" } as any,
		"input-a",
	);
	const rejected = assert.rejects(first, /unconfirmed/);
	await reviewUntil(() => sent("dispatchAction").length === 1);
	t.mock.timers.tick(30_000);
	await rejected;
	// A retry carrying different data still sends the original action.
	await ahp.dispatch(
		reviewTerminalURI,
		{ type: "terminal/input", data: "changed" } as any,
		"input-a",
	);
	await ahp.dispatch(
		reviewTerminalURI,
		{ type: "terminal/input", data: "b" } as any,
		"input-b",
	);
	const [original, retry, next] = sent("dispatchAction").map((m) => m.params);
	assert.deepEqual(retry, original);
	assert.equal(next.clientSeq, original.clientSeq + 1);
	assert.equal(next.action.data, "b");
});

test("unconfirmed actions expire after the retry window", async (t) => {
	t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
	let drop = true;
	const { ahp, sent } = await reviewConnection(t, async (m, peer) => {
		if (m.method === "dispatchAction" && !drop)
			await reviewAck(peer, m, ahp, sent("dispatchAction").length);
	});
	const lost = assert.rejects(
		ahp.dispatch(
			reviewTerminalURI,
			{ type: "terminal/input", data: "a" } as any,
			"stale",
		),
		/unconfirmed/,
	);
	await reviewUntil(() => sent("dispatchAction").length === 1);
	t.mock.timers.tick(30_000);
	await lost;
	t.mock.timers.tick(5 * 60_000);
	drop = false;
	// Pruning happens when a new action is registered.
	await ahp.dispatch(
		reviewTerminalURI,
		{ type: "terminal/input", data: "b" } as any,
		"fresh",
	);
	await ahp.dispatch(
		reviewTerminalURI,
		{ type: "terminal/input", data: "c" } as any,
		"stale",
	);
	const seqs = sent("dispatchAction").map((m) => [
		m.params.action.data,
		m.params.clientSeq,
	]);
	assert.deepEqual(seqs, [
		["a", 1],
		["b", 2],
		["c", 3],
	]);
});

test("a lost connection fails reads promptly while watches resume with a new snapshot", async (t) => {
	let subscribes = 0;
	const { ahp, peers } = await reviewConnection(t, async (m, peer) => {
		if (m.method !== "subscribe") return;
		// The second channel never answers on the first connection.
		if (m.params.channel === "ahp-terminal:/slow" && subscribes++ === 0) return;
		await peer.reply(m.id, {
			snapshot: {
				resource: m.params.channel,
				fromSeq: 0,
				state: reviewTerminal(String(peers.length)),
			},
		});
	});
	const seen: string[] = [];
	const release = ahp.watch(reviewTerminalURI, (update) =>
		seen.push(reviewText(update.state)),
	);
	await reviewUntil(() => seen.length === 1);
	assert.equal(
		reviewText(await ahp.read(reviewTerminalURI)),
		"1",
		"read reuses the live observer",
	);
	const pending = ahp.read("ahp-terminal:/slow");
	await setImmediate();
	const failed = assert.rejects(pending, /Connection lost/);
	await peers[0].server.close();
	await failed;
	await ahp.ready();
	await reviewUntil(() => seen.length === 2, "resubscribe");
	assert.deepEqual(seen, ["1", "2"]);
	assert.equal(reviewText(await ahp.read("ahp-terminal:/slow")), "2");
	release();
});

test("a watch released mid-subscribe ignores its late snapshot and the next watch subscribes again", async (t) => {
	const held: any[] = [];
	const { ahp, sent } = await reviewConnection(t, async (m, peer) => {
		if (m.method === "subscribe") held.push({ m, peer });
	});
	const stale: unknown[] = [];
	const first = ahp.watch(reviewTerminalURI, (update) => stale.push(update));
	await reviewUntil(() => held.length === 1);
	first();
	const fresh: ChannelUpdate[] = [];
	const second = ahp.watch(reviewTerminalURI, (update) => fresh.push(update));
	await reviewUntil(() => held.length === 2);
	const [old, current] = held;
	await old.peer.reply(old.m.id, {
		snapshot: {
			resource: reviewTerminalURI,
			fromSeq: 1,
			state: reviewTerminal("old"),
		},
	});
	await old.peer.data(2, "!");
	await current.peer.reply(current.m.id, {
		snapshot: {
			resource: reviewTerminalURI,
			fromSeq: 2,
			state: reviewTerminal("new"),
		},
	});
	await current.peer.data(3, "?");
	await reviewUntil(() => fresh.length === 2);
	assert.deepEqual(stale, []);
	assert.equal(reviewText(fresh[1].state), "new?");
	assert.deepEqual(
		sent("unsubscribe").map((m) => m.params.channel),
		[reviewTerminalURI],
	);
	second();
});

test("a channel that floods before its snapshot drops the connection, which recovers", async (t) => {
	let connection = 0;
	const { ahp, peers } = await reviewConnection(t, async (m, peer) => {
		if (m.method !== "subscribe") return;
		if (connection++ === 0) {
			for (let seq = 1; seq <= 4097; seq++) await peer.data(seq, ".");
			return;
		}
		await peer.reply(m.id, {
			snapshot: {
				resource: m.params.channel,
				fromSeq: 0,
				state: reviewTerminal("ok"),
			},
		});
	});
	let state: any;
	const release = ahp.watch(
		reviewTerminalURI,
		(update) => (state = update.state),
	);
	await reviewUntil(() => !ahp.getConnected(), "shutdown");
	await ahp.ready();
	await reviewUntil(() => state !== undefined);
	assert.equal(peers.length, 2);
	assert.equal(reviewText(state), "ok");
	release();
});

test("a replaced workspace stops reconnecting and fails new work", async (t) => {
	let instance = reviewScope.instanceId;
	const { ahp, peers } = await reviewConnection(t, () => {}, {
		instance: () => instance,
	});
	instance = "replacement";
	await peers[0].server.close();
	await reviewUntil(() => !ahp.getConnected(), "disconnect");
	await assert.rejects(ahp.ready(), /Workspace changed/);
	assert.equal(ahp.getReplaced(), true);
	await assert.rejects(ahp.read(reviewTerminalURI), /Workspace changed/);
	await assert.rejects(
		ahp.dispatch(reviewTerminalURI, {
			type: "terminal/input",
			data: "x",
		} as any),
		/Workspace changed/,
	);
	await new Promise((resolve) => setTimeout(resolve, 1100));
	assert.equal(peers.length, 2, "no retry after the replacement was detected");
});

test("changeset actions invalidate their layer; snapshots and other channels do not", async (t) => {
	const { ahp } = await reviewConnection(t, async (m, peer) => {
		if (m.method !== "subscribe") return;
		await peer.reply(m.id, {
			snapshot: {
				resource: m.params.channel,
				fromSeq: 0,
				state: { status: "ready", files: [] },
			},
		});
		const file = {
			id: "file:///workspace/a",
			edit: {
				after: {
					uri: "file:///workspace/a",
					content: { uri: "ahp-content:/1" },
				},
			},
		};
		await peer.send({
			jsonrpc: "2.0",
			method: "action",
			params: {
				channel: m.params.channel,
				serverSeq: 1,
				action: { type: "changeset/fileSet", file },
			},
		});
		await peer.send({
			jsonrpc: "2.0",
			method: "action",
			params: {
				channel: m.params.channel,
				serverSeq: 2,
				action: { type: "changeset/operationsChanged", operations: [] },
			},
		});
	});
	const events: string[] = [];
	ahp.subscribe((event) => events.push(event.type));
	let updates = 0;
	const releases = ["uncommitted", "staged", "unstaged"].map((layer) =>
		ahp.watch(`ahp-changeset:/wingman/${layer}`, () => updates++),
	);
	await reviewUntil(() => updates === 9);
	assert.deepEqual(events.sort(), [
		"diffs_changed",
		"git_index_changed",
		"git_index_changed",
	]);
	for (const release of releases) release();
});

test("malformed frames are skipped without dropping the connection", async (t) => {
	t.mock.method(console, "warn", () => {});
	const { ahp, peers } = await reviewConnection(t, async (m, peer) => {
		if (m.method !== "subscribe") return;
		await peer.server.send("{not json");
		await peer.reply(m.id, {
			snapshot: {
				resource: m.params.channel,
				fromSeq: 0,
				state: reviewTerminal("ok"),
			},
		});
	});
	const state = await ahp.read(reviewTerminalURI);
	assert.equal(reviewText(state), "ok");
	assert.equal(ahp.getConnected(), true);
	assert.equal(peers.length, 1);
});

// Bug: the standard changeset/contentChanged and changeset/cleared actions replace a layer's file list without emitting diffs_changed or git_index_changed, so diff and Git queries stay stale; every action that changes the file list should invalidate its layer.
test("whole-list changeset actions invalidate their layer", async (t) => {
	const { ahp } = await reviewConnection(t, async (m, peer) => {
		if (m.method !== "subscribe") return;
		const file = {
			id: "file:///workspace/a",
			edit: {
				after: {
					uri: "file:///workspace/a",
					content: { uri: "ahp-content:/1" },
				},
			},
		};
		await peer.reply(m.id, {
			snapshot: {
				resource: m.params.channel,
				fromSeq: 0,
				state: { status: "ready", files: [file] },
			},
		});
		const action = m.params.channel.endsWith("/uncommitted")
			? { type: "changeset/contentChanged", files: [] }
			: { type: "changeset/cleared" };
		await peer.send({
			jsonrpc: "2.0",
			method: "action",
			params: { channel: m.params.channel, serverSeq: 1, action },
		});
	});
	const events: string[] = [];
	ahp.subscribe((event) => events.push(event.type));
	const files: number[] = [];
	const releases = ["uncommitted", "staged"].map((layer) =>
		ahp.watch(`ahp-changeset:/wingman/${layer}`, (update) =>
			files.push((update.state as any).files.length),
		),
	);
	await reviewUntil(() => files.length === 4);
	assert.deepEqual(
		files.sort(),
		[0, 0, 1, 1],
		"the reducer emptied both layers",
	);
	assert.deepEqual(events.sort(), ["diffs_changed", "git_index_changed"]);
	for (const release of releases) release();
});

// Bug: a dispatch issued while connected but resumed after stop() (or a dropped connection) dereferences the cleared client and rejects with an internal TypeError; it should reject with the workspace's disconnected/stopped error.
test("a dispatch racing stop rejects with a disconnection error", async (t) => {
	const { ahp } = await reviewConnection(t, () => {});
	const dispatched = ahp.dispatch(reviewTerminalURI, {
		type: "terminal/input",
		data: "x",
	} as any);
	ahp.stop();
	await assert.rejects(dispatched, (error: Error) => {
		assert.doesNotMatch(String(error), /TypeError|undefined/);
		assert.match(error.message, /stopped|disconnected|lost/i);
		return true;
	});
});
