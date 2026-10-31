import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { setImmediate } from "node:timers/promises";
import { InMemoryTransport } from "@microsoft/agent-host-protocol/client";
import { AHPConnection } from "../src/state/ahpConnection.ts";
import { WorkspaceClient } from "../src/state/workspaceClient.ts";
import { canonicalChatURI } from "../src/state/sessionStore.ts";

const scope = { workspaceId: "workspace", instanceId: "instance" };
const sessionURI = "ahp-session:/s1",
	defaultChat = "ahp-chat:/s1",
	forkChat = "ahp-chat:/fork";
const turn = (id: string) => ({
	id,
	message: { text: id, origin: { kind: "user" } },
	responseParts: [],
	usage: undefined,
	state: "complete",
});
function chatState(resource: string, extra: Record<string, unknown> = {}) {
	return {
		resource,
		title: "Chat",
		status: 1,
		modifiedAt: "",
		turns: [turn("t1"), turn("t2")],
		...extra,
	};
}
const session = {
	provider: "one",
	title: "Chat",
	status: 1,
	lifecycle: "ready",
	activeClients: [],
	chats: [
		{ resource: defaultChat, title: "Chat", status: 1, modifiedAt: "" },
		{ resource: forkChat, title: "Fork", status: 1, modifiedAt: "" },
	],
	defaultChat,
};

async function host(t: TestContext, states: Record<string, unknown>) {
	const requests: any[] = [];
	let seq = 0;
	const ahp = new AHPConnection(scope, async () => {
		const [client, server] = InMemoryTransport.pair();
		const reply = (id: number, body: Record<string, unknown>) =>
			server.send(JSON.stringify({ jsonrpc: "2.0", id, ...body }));
		void (async () => {
			for (;;) {
				const frame = await server.recv();
				if (!frame) return;
				const m =
					frame.kind === "text"
						? JSON.parse(frame.text)
						: frame.kind === "parsed"
							? frame.message
							: {};
				requests.push(m);
				if (m.method === "initialize")
					await reply(m.id, {
						result: {
							protocolVersion: "1.1.0",
							serverSeq: 0,
							defaultDirectory: "file:///workspace",
							snapshots: [
								{
									resource: "ahp-root://",
									fromSeq: 0,
									state: { agents: [], _meta: { "wingman.workspace": scope } },
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
					else
						await reply(m.id, {
							result: {
								snapshot: { resource: m.params.channel, fromSeq: 0, state },
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
									resource: sessionURI,
									provider: "one",
									title: "Chat",
									status: 1,
									createdAt: "",
									modifiedAt: "",
									defaultChat,
									chats: session.chats,
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
	const client = new WorkspaceClient({ ...scope, protocol: 2 }, ahp);
	t.after(() => client.stop());
	const sent = (method: string) => requests.filter((m) => m.method === method);
	return { client, ahp, sent };
}

test("confirmations send the selected option and settings change the chat", async (t) => {
	const pending = chatState(forkChat, {
		activeTurn: {
			id: "t3",
			startedAt: "",
			message: { text: "go", origin: { kind: "user" } },
			usage: undefined,
			responseParts: [
				{
					kind: "toolCall",
					toolCall: {
						toolCallId: "approval:1",
						toolName: "bash",
						displayName: "Bash",
						invocationMessage: "Run",
						status: "pending-confirmation",
						options: [
							{ id: "once", label: "Allow", kind: "approve" },
							{
								id: "session",
								label: "Allow for this session",
								kind: "approve",
								group: 1,
							},
							{ id: "deny", label: "Deny", kind: "deny" },
						],
					},
				},
			],
		},
	});
	const { client, sent } = await host(t, {
		[sessionURI]: session,
		[forkChat]: pending,
	});
	await client.respond(forkChat, "approval:1", {
		action: "accept",
		option: "session",
	});
	await client.respond(forkChat, "approval:1", { action: "decline" });
	await client.configure(forkChat, { model: "large", effort: undefined });
	const actions = sent("dispatchAction").map((m) => [
		m.params.channel,
		m.params.action,
	]);
	assert.deepEqual(actions, [
		[
			forkChat,
			{
				type: "chat/toolCallConfirmed",
				turnId: "t3",
				toolCallId: "approval:1",
				approved: true,
				confirmed: "user-action",
				selectedOptionId: "session",
			},
		],
		[
			forkChat,
			{
				type: "chat/toolCallConfirmed",
				turnId: "t3",
				toolCallId: "approval:1",
				approved: false,
				reason: "denied",
				selectedOptionId: "deny",
			},
		],
		[forkChat, { type: "chat/configChanged", config: { model: "large" } }],
	]);
});

test("forks are chats of the session; disposal follows the chat's role", async (t) => {
	const { client, sent } = await host(t, {
		[sessionURI]: session,
		[defaultChat]: chatState(defaultChat),
		[forkChat]: chatState(forkChat),
	});
	const created = await client.fork(defaultChat, "Try B");
	assert.match(created, /^ahp-chat:\/[0-9a-f-]{36}$/);
	assert.deepEqual(sent("createChat")[0].params, {
		channel: sessionURI,
		chat: created,
		source: { kind: "fork", chat: defaultChat, turnId: "t2" },
		title: "Try B",
	});
	await client.fork(forkChat, undefined, "t1");
	assert.deepEqual(sent("createChat")[1].params.source, {
		kind: "fork",
		chat: forkChat,
		turnId: "t1",
	});
	assert.equal(await client.sessionOf(created), sessionURI);
	await client.dispose(forkChat);
	await client.dispose(defaultChat);
	assert.deepEqual(
		sent("disposeChat").map((m) => m.params.channel),
		[forkChat],
	);
	assert.deepEqual(
		sent("disposeSession").map((m) => m.params.channel),
		[sessionURI],
	);
	// The fork was resolved through the catalogue once.
	assert.equal(sent("listSessions").length, 1);
});

test("stop cancels the turn, clearing removes each queued message, compact is a message", async (t) => {
	const busy = chatState(defaultChat, {
		activeTurn: {
			id: "t3",
			startedAt: "",
			message: { text: "go", origin: { kind: "user" } },
			usage: undefined,
			responseParts: [],
		},
		steeringMessage: {
			id: "s",
			kind: "steering",
			message: { text: "s", origin: { kind: "user" } },
		},
		queuedMessages: [
			{
				id: "q",
				kind: "queued",
				message: { text: "q", origin: { kind: "user" } },
			},
		],
	});
	const { client, sent } = await host(t, {
		[sessionURI]: session,
		[defaultChat]: busy,
	});
	await client.cancel(defaultChat);
	await client.clearQueue(defaultChat);
	await client.compact(defaultChat, "keep tests");
	const actions = sent("dispatchAction").map((m) => m.params.action);
	assert.deepEqual(actions.slice(0, 3), [
		{ type: "chat/turnCancelled", turnId: "t3", duration: 0 },
		{ type: "chat/pendingMessageRemoved", kind: "steering", id: "s" },
		{ type: "chat/pendingMessageRemoved", kind: "queued", id: "q" },
	]);
	assert.equal(actions[3].type, "chat/pendingMessageSet");
	assert.equal(actions[3].kind, "queued");
	assert.equal(actions[3].message.text, "/compact keep tests");
});

test("a migrated chat follows the host's alias and the view tracks its session", async (t) => {
	const canonical = canonicalChatURI("one", "native");
	const canonicalSession = canonical.replace("ahp-chat:", "ahp-session:");
	const { client } = await host(t, {
		[canonicalSession]: sessionURI,
		[canonical]: defaultChat,
		[sessionURI]: session,
		[defaultChat]: chatState(defaultChat),
	});
	const redirects: [string, string][] = [];
	client.onRedirect((from, to) => redirects.push([from, to]));
	const release = client.watch(canonical);
	for (let i = 0; i < 10 && !redirects.length; i++) await setImmediate();
	assert.deepEqual(redirects, [[canonical, defaultChat]]);
	release();
	const watched = client.watch(defaultChat);
	for (
		let i = 0;
		i < 10 && !client.store.getSnapshot()[defaultChat]?.synchronized;
		i++
	)
		await setImmediate();
	const view = client.store.getSnapshot()[defaultChat];
	assert.equal(view.session, sessionURI);
	assert.equal(view.provider, "one");
	assert.equal(client.backendOf(defaultChat), "one");
	assert.equal(view.entries.length, 2);
	watched();
});
