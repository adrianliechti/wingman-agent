import assert from "node:assert/strict";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { writeFile, mkdir } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { AhpClient } from "@microsoft/agent-host-protocol/client";
import { AhpClient as V1Client } from "@microsoft/agent-host-protocol-v1/client";
import { WebSocketTransport as V1Transport } from "@microsoft/agent-host-protocol-v1/ws";
import { chatReducer as v1ChatReducer } from "@microsoft/agent-host-protocol-v1";
import { WebSocketTransport } from "@microsoft/agent-host-protocol/ws";
import {
	terminalReducer,
	chatReducer,
	changesetReducer,
} from "@microsoft/agent-host-protocol";
import { AHPConnection } from "../src/state/ahpConnection.ts";
import { WorkspaceClient } from "../src/state/workspaceClient.ts";

const endpoint = process.env.AHP_TEST_URL!;
const base = process.env.AHP_TEST_HTTP!;
const root = process.env.AHP_TEST_ROOT!;
const scope = JSON.parse(process.env.AHP_TEST_SCOPE!);
async function until(predicate: () => unknown, label = "condition") {
	const deadline = Date.now() + 10000;
	while (!predicate()) {
		if (Date.now() > deadline)
			throw new Error(`Timed out waiting for ${label}`);
		await delay(10);
	}
}
async function peer(id: string) {
	const transport = await WebSocketTransport.connect(endpoint);
	const client = new AhpClient(transport);
	client.connect();
	const result = await client.initialize({
		clientId: id,
		protocolVersions: ["1.1.0", "1.0.0"],
		initialSubscriptions: ["ahp-root://"],
	});
	assert.equal(result.protocolVersion, "1.1.0");
	return client;
}
async function observe(
	client: AhpClient | V1Client,
	uri: string,
	reducer: (state: any, action: any) => any,
) {
	const { result, subscription } = await client.subscribe(uri);
	let state = result.snapshot!.state;
	const actions: any[] = [];
	const loop = (async () => {
		for await (const event of subscription)
			if (event.type === "action") {
				actions.push(event.params);
				if (!event.params.rejectionReason)
					state = reducer(state, event.params.action);
			}
	})();
	return {
		get state() {
			return state as any;
		},
		actions,
		close: async () => {
			await client.unsubscribe(uri);
			await subscription.close();
			await loop;
		},
	};
}

test(
	"browser facade and official SDK interoperate with the Go host",
	{ timeout: 60000 },
	async (t) => {
		const ahp = new AHPConnection(scope, () =>
			WebSocketTransport.connect(endpoint),
		);
		const browser = new WorkspaceClient(scope, ahp);
		browser.start();
		t.after(() => browser.stop());
		await ahp.ready();
		const vscode = await peer("vscode-test");
		t.after(() => vscode.shutdown());
		await t.test(
			"standard resources and browser file facade preserve content and conflicts",
			async () => {
				const path = "resource-fixture/deep/file 🌍.txt";
				const directory = pathToFileURL(`${root}/resource-fixture/deep`).href;
				await vscode.resourceMkdir({ uri: directory });
				await vscode.resourceMkdir({ uri: directory });
				const created = await browser.files.create(path, {
					content: "hello 🌍\n",
				});
				assert.equal(created?.content, "hello 🌍\n");
				assert.ok(
					(await browser.files.list("resource-fixture/deep")).some(
						(file) => file.name === "file 🌍.txt",
					),
				);
				const read = await browser.files.read(path);
				const metadata = await vscode.resourceResolve({
					uri: pathToFileURL(`${root}/${path}`).href,
				});
				assert.equal(read.revision, metadata.etag);
				await browser.files.write({
					path,
					content: "updated",
					revision: read.revision,
				});
				await assert.rejects(
					browser.files.write({
						path,
						content: "stale",
						revision: read.revision,
					}),
					(error: any) => error.code === -32011,
				);
				await browser.files.copy(path, "resource-copy.txt");
				await assert.rejects(
					browser.files.copy(path, "resource-copy.txt"),
					(error: any) => error.code === -32010,
				);
				await browser.files.move("resource-copy.txt", "resource-moved.txt");
				assert.equal(
					(await browser.files.read("resource-moved.txt")).content,
					"updated",
				);
				assert.ok(
					(await browser.files.search("resource-moved")).some(
						(file) => file.path === "resource-moved.txt",
					),
				);
				await browser.files.delete("resource-fixture");
				await browser.files.delete("resource-fixture");
				await assert.rejects(
					browser.files.read(path),
					(error: any) => error.code === -32008,
				);
				await browser.files.delete("resource-moved.txt");
			},
		);

		let key = "",
			uri = "",
			chatURI = "";
		await t.test(
			"creation is shared and idempotent; catalog carries native identity",
			async () => {
				[key] = await Promise.all([
					browser.create("one", "draft"),
					browser.create("one", "draft"),
				]);
				assert.equal(await browser.create("one", "draft"), key);
				uri = await browser.resource(key);
				const session = await ahp.read<any>(uri);
				chatURI = session.defaultChat;
				assert.equal(session._meta.wingman.ref.backendId, "one");
				const list = await browser.list("one");
				assert.equal(list.filter((s) => s.id === key).length, 1);
			},
		);
		const release = browser.watch(key);
		t.after(release);
		const chat = await observe(vscode, chatURI, chatReducer);
		t.after(chat.close);
		const legacy = new V1Client(await V1Transport.connect(endpoint));
		legacy.connect();
		t.after(() => legacy.shutdown());
		const initialized = await legacy.initialize({
			clientId: "vscode-1.0",
			protocolVersions: ["1.0.0"],
		});
		assert.equal(initialized.protocolVersion, "1.0.0");
		const legacyChat = await observe(legacy, chatURI, v1ChatReducer);
		t.after(legacyChat.close);
		await t.test(
			"saved session subscriptions may mount before the workspace connection starts",
			async () => {
				const transport = new AHPConnection(scope, () =>
					WebSocketTransport.connect(endpoint),
				);
				const restored = new WorkspaceClient(scope, transport);
				const unwatch = restored.watch(key);
				restored.start();
				try {
					await until(
						() => restored.store.getSnapshot()[key]?.synchronized,
						"restored session before provider effect",
					);
				} finally {
					unwatch();
					restored.stop();
				}
			},
		);
		await t.test(
			"streaming from browser appears in the SDK, without duplicate sends",
			async () => {
				await Promise.all([
					browser.send(key, "hello"),
					browser.send(key, "hello"),
				]);
				await until(
					() => chat.state.activeTurn?.responseParts.length,
					"streamed answer",
				);
				await until(
					() =>
						browser.store
							.getSnapshot()
							[key]?.entries.some((e) => e.content === "live prefix"),
					"browser projection",
				);
				assert.equal(chat.state.activeTurn.message.text, "hello");
				assert.equal(
					browser.store
						.getSnapshot()
						[key].entries.filter((e) => e.type === "user").length,
					1,
				);
			},
		);
		await t.test("approvals resolve once across clients", async () => {
			const sid = JSON.parse(key)[1];
			const response = fetch(`${base}/fixture/confirm?session=${sid}`, {
				method: "POST",
			});
			await until(
				() =>
					browser.store
						.getSnapshot()
						[key]?.prompts.some((p) => p.kind === "confirm"),
				"approval",
			);
			const prompt = browser.store
				.getSnapshot()
				[key].prompts.find((p) => p.kind === "confirm")!;
			await browser.command(key, {
				type: "prompt_response",
				promptId: prompt.id,
				action: "accept",
			});
			assert.deepEqual(await (await response).json(), { accepted: true });
			await until(
				() => !browser.store.getSnapshot()[key].prompts.length,
				"approval completion",
			);
			await assert.rejects(
				browser.command(key, {
					type: "prompt_response",
					promptId: prompt.id,
					action: "accept",
				}),
				/resolved|expired|active/,
			);
		});
		await t.test(
			"typed input requests and answers survive the browser mapping",
			async () => {
				const response = fetch(
					`${base}/fixture/input?session=${JSON.parse(key)[1]}`,
					{ method: "POST" },
				);
				await until(
					() =>
						browser.store
							.getSnapshot()
							[key]?.prompts.some((p) => p.kind === "ask"),
					"input request",
				);
				const prompt = browser.store
					.getSnapshot()
					[key].prompts.find((p) => p.kind === "ask")!;
				assert.deepEqual(prompt.fields?.[0].enum, ["one", "two"]);
				await browser.command(key, {
					type: "prompt_response",
					promptId: prompt.id,
					action: "accept",
					content: { choice: "two" },
				});
				const result = await (await response).json();
				assert.equal(result.content.choice, "two");
			},
		);
		await t.test(
			"pending messages edit and remove through standard AHP actions",
			async () => {
				await browser.send(key, "queued");
				await until(
					() =>
						chat.state.queuedMessages?.some(
							(p: any) => p.message.text === "queued",
						),
					"queue",
				);
				await until(
					() =>
						legacyChat.state.queuedMessages?.some(
							(p: any) => p.message.text === "queued",
						),
					"1.0 queue",
				);
				assert.equal(chat.state.queuedMessages[0].kind, "queued");
				const id = chat.state.queuedMessages[0].id;
				await browser.command(key, {
					type: "queue_update",
					inputId: id,
					text: "edited",
					intent: "follow_up",
				});
				await until(
					() => chat.state.queuedMessages[0]?.message.text === "edited",
					"queue update",
				);
				await browser.command(key, { type: "queue_remove", inputId: id });
				await until(() => !chat.state.queuedMessages?.length, "queue removal");
			},
		);
		await t.test(
			"settings and complete turn state are reflected in both clients",
			async () => {
				await browser.command(key, { type: "settings", model: "test-model" });
				await until(
					() =>
						browser.store.getSnapshot()[key].settings.model === "test-model",
				);
				await fetch(`${base}/fixture/release`, { method: "POST" });
				await until(
					() => !chat.state.activeTurn && chat.state.turns.length === 1,
					"turn completion",
				);
				assert.equal(chat.state.turns[0].message.text, "hello");
			},
		);
		await t.test(
			"reconnect recovers history and keeps browser operations usable",
			async () => {
				browser.stop();
				browser.start();
				await ahp.ready();
				await until(
					() => browser.store.getSnapshot()[key]?.synchronized,
					"browser reconnect",
				);
				assert.equal(
					browser.store
						.getSnapshot()
						[key].entries.filter((e) => e.type === "user").length,
					1,
				);
				await browser.send(key, "after reconnect");
				await until(() => chat.state.turns.length === 2, "reconnected turn");
			},
		);
		await t.test(
			"terminal output, UTF-8, resize, ownership, exit and late snapshots",
			async () => {
				const terminalURI = "ahp-terminal:/typescript-test";
				await ahp.request("createTerminal", {
					channel: terminalURI,
					cols: 90,
					rows: 30,
					claim: { kind: "client", clientId: ahp.clientId } as any,
				});
				const terminal = await observe(vscode, terminalURI, terminalReducer);
				try {
					await ahp.dispatch(terminalURI, {
						type: "terminal/resized",
						cols: 100,
						rows: 35,
					} as any);
					await until(() => terminal.state.cols === 100);
					vscode.dispatch(
						terminalURI,
						{ type: "terminal/input", data: "echo forbidden\n" } as any,
						1,
					);
					await until(
						() =>
							terminal.actions.some(
								(a) =>
									a.origin?.clientId === "vscode-test" && a.rejectionReason,
							),
						"ownership rejection",
					);
					await ahp.dispatch(terminalURI, {
						type: "terminal/input",
						data: "printf 'terminal-🌍-ok\\n'; exit 7\n",
					} as any);
					await until(
						() => terminal.state.lifecycle.status === "exited",
						"terminal exit",
					);
					assert.equal(terminal.state.lifecycle.exitCode, 7);
					assert.match(
						terminal.state.content
							.map((p: any) => p.value ?? p.output)
							.join(""),
						/terminal-🌍-ok/,
					);
					const recovered = await ahp.read<any>(terminalURI);
					assert.equal(recovered.lifecycle.exitCode, 7);
					assert.ok(recovered.content.length);
					await ahp.request("disposeTerminal", { channel: terminalURI });
					await assert.rejects(ahp.read(terminalURI), /not found/);
				} finally {
					await terminal.close();
				}
			},
		);
		await t.test(
			"resource watches report nested changes and honor exclusions",
			async () => {
				const fileURI = pathToFileURL(root).href;
				const result = await vscode.request("createResourceWatch", {
					channel: "ahp-root://",
					uri: fileURI,
					recursive: true,
					excludes: { items: ["**/*.ignore"] },
				});
				const watch = await observe(vscode, result.channel, (state) => state);
				try {
					await mkdir(`${root}/nested`);
					await writeFile(`${root}/nested/watched.txt`, "hello");
					await writeFile(`${root}/nested/skip.ignore`, "ignored");
					await until(
						() =>
							watch.actions.some((a) =>
								a.action.changes?.items.some((c: any) =>
									c.uri.endsWith("watched.txt"),
								),
							),
						"resource change",
					);
					assert.ok(
						!watch.actions.some((a) =>
							a.action.changes?.items.some((c: any) =>
								c.uri.endsWith("skip.ignore"),
							),
						),
					);
				} finally {
					await watch.close();
				}
			},
		);
		await t.test(
			"changeset channel and file reads are accessible to the official SDK",
			async () => {
				await writeFile(`${root}/tracked.txt`, "updated\n");
				const changes = await observe(
					vscode,
					"ahp-changeset:/wingman/uncommitted",
					changesetReducer,
				);
				try {
					assert.equal(changes.state.status, "ready");
					const file = changes.state.files.find((file: any) =>
						file.id.endsWith("tracked.txt"),
					);
					assert.ok(file?.nonce);
					assert.ok(file.edit.before.content.nonce);
					assert.ok(file.edit.after.content.nonce);
					assert.equal(
						(await vscode.resourceRead({ uri: file.edit.before.content.uri }))
							.data,
						"original\n",
					);
					assert.equal(
						(await vscode.resourceRead({ uri: file.edit.after.content.uri }))
							.data,
						"updated\n",
					);
					assert.equal(file.isEditing, false);
					await writeFile(`${root}/tracked.txt`, "changed again\n");
					await until(
						() =>
							changes.state.files.find((f: any) => f.id === file.id)?.nonce !==
							file.nonce,
						"changeset nonce update",
					);
				} finally {
					await changes.close();
				}
				const content = await vscode.resourceRead({
					uri: pathToFileURL(`${root}/nested/watched.txt`).href,
				});
				assert.equal(content.data, "hello");
				await assert.rejects(
					vscode.resourceRead({ uri: "file:///etc/passwd" }),
					/outside/,
				);
			},
		);
	},
);
