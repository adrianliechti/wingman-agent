import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { setImmediate } from "node:timers/promises";
import {
	InMemoryTransport,
	type AhpTransport,
} from "@microsoft/agent-host-protocol/client";
import { AHPConnection } from "../src/state/ahpConnection.ts";
import { WorkspaceClient } from "../src/state/workspaceClient.ts";
import { canonicalChatURI } from "../src/state/sessionStore.ts";

const reviewScope = { workspaceId: "workspace", instanceId: "instance" };
const reviewSessionURI = "ahp-session:/s1",
	reviewDefaultChat = "ahp-chat:/s1";
const reviewTurn = (id: string) => ({
	id,
	message: { text: id, origin: { kind: "user" } },
	responseParts: [],
	usage: undefined,
	state: "complete",
});
function reviewChat(resource: string, extra: Record<string, unknown> = {}) {
	return {
		resource,
		title: "Chat",
		status: 1,
		modifiedAt: "",
		turns: [reviewTurn("t1")],
		...extra,
	};
}
const reviewSession = {
	provider: "one",
	title: "Chat",
	status: 1,
	lifecycle: "ready",
	activeClients: [],
	chats: [
		{ resource: reviewDefaultChat, title: "Chat", status: 1, modifiedAt: "" },
	],
	defaultChat: reviewDefaultChat,
};

/** A scripted host; a string state answers subscribe with the host's alias error. */
async function reviewHost(
	t: TestContext,
	states: Record<string, unknown>,
	onMessage?: (
		message: any,
		server: AhpTransport,
	) => Promise<boolean> | boolean,
) {
	const requests: any[] = [];
	let seq = 0;
	const ahp = new AHPConnection(reviewScope, async () => {
		const [client, server] = InMemoryTransport.pair();
		const reply = (id: number, body: Record<string, unknown>) =>
			server.send(JSON.stringify({ jsonrpc: "2.0", id, ...body }));
		void (async () => {
			for (;;) {
				const frame = await server.recv();
				if (!frame) return;
				const m = frame.kind === "text" ? JSON.parse(frame.text) : {};
				requests.push(m);
				if (onMessage && (await onMessage(m, server))) continue;
				if (m.method === "initialize")
					await reply(m.id, {
						result: {
							protocolVersion: "1.1.0",
							serverSeq: seq,
							defaultDirectory: "file:///workspace",
							snapshots: [
								{
									resource: "ahp-root://",
									fromSeq: seq,
									state: {
										agents: [],
										_meta: { "wingman.workspace": reviewScope },
									},
								},
							],
						},
					});
				else if (m.method === "subscribe") {
					const state = states[m.params.channel];
					if (typeof state === "string")
						await reply(m.id, {
							error: {
								code: -32602,
								message: "aliased",
								data: { resource: state },
							},
						});
					else if (state === undefined)
						await reply(m.id, {
							error: { code: -32008, message: "not found" },
						});
					else
						await reply(m.id, {
							result: {
								snapshot: { resource: m.params.channel, fromSeq: seq, state },
							},
						});
				} else if (m.method === "dispatchAction")
					await server.send(
						JSON.stringify({
							jsonrpc: "2.0",
							method: "action",
							params: {
								channel: m.params.channel,
								action: m.params.action,
								serverSeq: ++seq,
								origin: {
									clientId: ahp.clientId,
									clientSeq: m.params.clientSeq,
								},
							},
						}),
					);
				else if (m.method === "listSessions")
					await reply(m.id, {
						result: {
							items: [
								{
									resource: reviewSessionURI,
									provider: "one",
									title: "Chat",
									status: 1,
									createdAt: "",
									modifiedAt: "",
									defaultChat: reviewDefaultChat,
									chats: reviewSession.chats,
								},
							],
						},
					});
				else if (m.id !== undefined) await reply(m.id, { result: {} });
			}
		})();
		return client;
	});
	t.after(() => ahp.stop());
	ahp.start();
	await ahp.ready();
	const client = new WorkspaceClient({ ...reviewScope, protocol: 2 }, ahp);
	t.after(() => client.stop());
	const sent = (method: string) => requests.filter((m) => m.method === method);
	return { client, ahp, sent };
}
async function reviewSettle(predicate: () => boolean) {
	for (let i = 0; i < 50 && !predicate(); i++) await setImmediate();
}

// Bug: once a legacy key's alias is known, watching that key again (a reopened link or a restored tab) marks the chat "deleted", which App answers by closing the tab, and never re-announces the redirect; it should redirect again and never report the chat deleted.
test("a legacy key watched again after its redirect is redirected, not deleted", async (t) => {
	const canonical = canonicalChatURI("one", "native");
	const canonicalSession = canonical.replace("ahp-chat:", "ahp-session:");
	const { client } = await reviewHost(t, {
		[canonicalSession]: reviewSessionURI,
		[canonical]: reviewDefaultChat,
		[reviewSessionURI]: reviewSession,
		[reviewDefaultChat]: reviewChat(reviewDefaultChat),
	});
	const redirects: [string, string][] = [];
	client.onRedirect((from, to) => redirects.push([from, to]));
	const first = client.watch(canonical);
	await reviewSettle(() => redirects.length > 0);
	assert.deepEqual(redirects, [[canonical, reviewDefaultChat]]);
	// The owner follows the redirect, as App does.
	first();
	const followed = client.watch(reviewDefaultChat);
	await reviewSettle(
		() => client.store.getSnapshot()[reviewDefaultChat]?.synchronized === true,
	);
	const again = client.watch(canonical);
	await reviewSettle(
		() =>
			redirects.length > 1 ||
			client.store.getSnapshot()[canonical]?.status === "deleted",
	);
	assert.notEqual(client.store.getSnapshot()[canonical]?.status, "deleted");
	assert.deepEqual(redirects.at(-1), [canonical, reviewDefaultChat]);
	again();
	followed();
});

// Bug: a session disposed by another client while this one was disconnected (its root/sessionRemoved is never replayed) resubscribes to SessionNotFound (-32001) and the chat shows a permanent error; it should be reported "deleted" like a live disposal so App closes the tab.
test("a chat disposed while disconnected is reported deleted after reconnecting", async (t) => {
	let disposed = false;
	const { client, ahp } = await reviewHost(
		t,
		{
			[reviewSessionURI]: reviewSession,
			[reviewDefaultChat]: reviewChat(reviewDefaultChat),
		},
		async (m, server) => {
			if (m.method !== "subscribe" || !disposed) return false;
			await server.send(
				JSON.stringify({
					jsonrpc: "2.0",
					id: m.id,
					error: {
						code: -32001,
						message: m.params.channel.startsWith("ahp-chat:")
							? "unknown chat"
							: "session disposed",
					},
				}),
			);
			return true;
		},
	);
	await client.sessionOf(reviewDefaultChat);
	const release = client.watch(reviewDefaultChat);
	const view = () => client.store.getSnapshot()[reviewDefaultChat];
	await reviewSettle(() => view()?.synchronized === true);
	ahp.stop();
	disposed = true;
	ahp.start();
	await ahp.ready();
	await reviewSettle(() => view().status !== "ready");
	assert.equal(view().status, "deleted", String(view().error));
	release();
});

test("an unconfirmed send is retried as the same pending message, whatever its intent", async (t) => {
	let drops = 1;
	const { client, ahp, sent } = await reviewHost(
		t,
		{
			[reviewSessionURI]: reviewSession,
			[reviewDefaultChat]: reviewChat(reviewDefaultChat),
		},
		async (m, server) => {
			if (m.method !== "dispatchAction" || drops-- <= 0) return false;
			await server.close();
			return true;
		},
	);
	await assert.rejects(
		client.send(reviewDefaultChat, "fix it", [], [], "steer"),
		/lost/,
	);
	await ahp.ready();
	await client.send(reviewDefaultChat, "fix it", [], [], "follow_up");
	await client.send(reviewDefaultChat, "fix it");
	const actions = sent("dispatchAction").map((m) => m.params);
	assert.equal(actions.length, 3);
	assert.deepEqual(
		actions[1],
		actions[0],
		"the retry repeats the original request",
	);
	assert.equal(actions[0].action.kind, "steering");
	assert.notEqual(
		actions[2].action.id,
		actions[0].action.id,
		"a later send is a new message",
	);
	assert.equal(actions[2].action.kind, "queued");
});

test("identical sends in flight coalesce into one delivery", async (t) => {
	const { client, sent } = await reviewHost(t, {
		[reviewSessionURI]: reviewSession,
		[reviewDefaultChat]: reviewChat(reviewDefaultChat),
	});
	await Promise.all([
		client.send(reviewDefaultChat, "go", ["a.ts"]),
		client.send(reviewDefaultChat, "go", ["a.ts"]),
		client.send(reviewDefaultChat, "go", ["b c.ts"]),
	]);
	const messages = sent("dispatchAction").map((m) => m.params.action);
	assert.equal(messages.length, 2);
	assert.deepEqual(
		messages.map((action) =>
			action.message.attachments.map((a: any) => [a.label, a.uri]),
		),
		[
			[["a.ts", "file:///workspace/a.ts"]],
			[["b c.ts", "file:///workspace/b%20c.ts"]],
		],
	);
});

test("creating and forking survive a lost reply without duplicates", async (t) => {
	const created = new Set<string>();
	let dropCreate = true,
		dropFork = true;
	const { client, ahp, sent } = await reviewHost(t, {}, async (m, server) => {
		const reply = (body: Record<string, unknown>) =>
			server.send(JSON.stringify({ jsonrpc: "2.0", id: m.id, ...body }));
		if (m.method === "createSession" || m.method === "createChat") {
			const uri = m.params.chat ?? m.params.channel;
			const drop = m.method === "createSession" ? dropCreate : dropFork;
			if (created.has(uri))
				await reply({ error: { code: -32003, message: "exists" } });
			else {
				created.add(uri);
				if (drop) {
					if (m.method === "createSession") dropCreate = false;
					else dropFork = false;
					await server.close();
					return true;
				}
				await reply({ result: {} });
			}
			return true;
		}
		const channel: string = m.params?.channel ?? "";
		if (
			m.method === "subscribe" &&
			channel.startsWith("ahp-session:/") &&
			created.has(channel)
		) {
			await reply({
				result: {
					snapshot: {
						resource: channel,
						fromSeq: 0,
						state: {
							...reviewSession,
							defaultChat: channel.replace("ahp-session:", "ahp-chat:"),
						},
					},
				},
			});
			return true;
		}
		if (m.method === "subscribe" && channel.startsWith("ahp-chat:/")) {
			await reply({
				result: {
					snapshot: {
						resource: channel,
						fromSeq: 0,
						state: reviewChat(channel),
					},
				},
			});
			return true;
		}
		return false;
	});
	await assert.rejects(client.create("one", "draft-1"));
	await ahp.ready();
	const chat = await client.create("one", "draft-1");
	assert.equal(
		await client.create("one", "draft-1"),
		chat,
		"a draft becomes one session",
	);
	assert.equal(
		new Set(sent("createSession").map((m) => m.params.channel)).size,
		1,
	);
	await assert.rejects(client.fork(chat, "B"));
	await ahp.ready();
	const fork = await client.fork(chat, "B");
	assert.deepEqual(
		sent("createChat").map((m) => m.params.chat),
		[fork, fork],
	);
	assert.equal(
		await client.sessionOf(fork),
		chat.replace("ahp-chat:", "ahp-session:"),
	);
	assert.notEqual(
		await client.fork(chat, "B"),
		fork,
		"a completed fork is not reused",
	);
});

test("a watched chat resynchronizes after a reconnect and reports its disposal", async (t) => {
	let turns = 1,
		chats = reviewSession.chats;
	const { client, ahp } = await reviewHost(t, {}, async (m, server) => {
		if (m.method !== "subscribe") return false;
		const state =
			m.params.channel === reviewSessionURI
				? { ...reviewSession, chats }
				: {
						...reviewChat(reviewDefaultChat),
						turns: Array.from({ length: turns }, (_, i) => reviewTurn(`t${i}`)),
					};
		await server.send(
			JSON.stringify({
				jsonrpc: "2.0",
				id: m.id,
				result: { snapshot: { resource: m.params.channel, fromSeq: 0, state } },
			}),
		);
		return true;
	});
	await client.sessionOf(reviewDefaultChat);
	const release = client.watch(reviewDefaultChat);
	const view = () => client.store.getSnapshot()[reviewDefaultChat];
	await reviewSettle(() => view()?.synchronized === true);
	assert.equal(view().entries.length, 1);
	turns = 3;
	ahp.stop();
	assert.equal(view().synchronized, false);
	assert.equal(
		view().entries.length,
		1,
		"history stays visible while disconnected",
	);
	ahp.start();
	await ahp.ready();
	await reviewSettle(
		() => view().synchronized === true && view().entries.length === 3,
	);
	assert.equal(view().entries.length, 3);
	assert.equal(view().status, "ready");
	// Another client disposed the chat: its session no longer lists it.
	chats = [
		{ resource: "ahp-chat:/other", title: "Other", status: 1, modifiedAt: "" },
	];
	ahp.stop();
	ahp.start();
	await ahp.ready();
	await reviewSettle(() => view().status === "deleted");
	assert.equal(view().status, "deleted");
	release();
});
