import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { InMemoryTransport } from "@microsoft/agent-host-protocol/client";
import { AHPConnection } from "../src/state/ahpConnection.ts";
import {
	ChangesetClient,
	workspaceChangeset,
} from "../src/state/changesetClient.ts";

const reviewScope = { workspaceId: "workspace", instanceId: "instance" };
const reviewRoot = "file:///work%20dir";
const reviewSide = (
	path: string,
	content: string,
	contentType = "text/plain",
) => ({
	uri: `${reviewRoot}/${path}`,
	content: { uri: `ahp-content:/${path}:${content.length}`, contentType },
});

async function reviewChangesets(
	t: TestContext,
	states: Record<string, any>,
	contents: Record<
		string,
		{ data: string; encoding?: string } | { error: number }
	>,
) {
	const requests: any[] = [];
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
				if (m.method === "initialize")
					await reply(m.id, {
						result: {
							protocolVersion: "1.1.0",
							serverSeq: 0,
							defaultDirectory: reviewRoot,
							snapshots: [
								{
									resource: "ahp-root://",
									fromSeq: 0,
									state: {
										agents: [],
										_meta: { "wingman.workspace": reviewScope },
									},
								},
							],
						},
					});
				else if (m.method === "subscribe")
					await reply(m.id, {
						result: {
							snapshot: {
								resource: m.params.channel,
								fromSeq: 0,
								state: states[m.params.channel],
							},
						},
					});
				else if (m.method === "resourceRead") {
					const content = contents[m.params.uri];
					if (!content)
						await reply(m.id, { error: { code: -32008, message: "gone" } });
					else if ("error" in content)
						await reply(m.id, {
							error: { code: content.error, message: "unreadable" },
						});
					else
						await reply(m.id, {
							result: {
								data: content.data,
								encoding: content.encoding ?? "utf-8",
								contentType: "text/plain",
							},
						});
				} else if (m.id !== undefined) await reply(m.id, { result: {} });
			}
		})();
		return client;
	});
	t.after(() => ahp.stop());
	ahp.start();
	await ahp.ready();
	const changesets = new ChangesetClient(ahp);
	t.after(() => changesets.close());
	const sent = (method: string) => requests.filter((m) => m.method === method);
	return { changesets, ahp, sent };
}

test("changeset files become git diffs: added, deleted, renamed, binary and unreadable", async (t) => {
	const files = [
		{
			id: `${reviewRoot}/new%20file.ts`,
			edit: { after: reviewSide("new%20file.ts", "a\n") },
		},
		{
			id: `${reviewRoot}/gone.ts`,
			edit: { before: reviewSide("gone.ts", "x") },
		},
		{
			id: `${reviewRoot}/dir/to.ts`,
			edit: {
				before: reviewSide("dir/from.ts", "same\n"),
				after: reviewSide("dir/to.ts", "same\nmore\n"),
			},
		},
		{
			id: `${reviewRoot}/image.png`,
			edit: {
				before: reviewSide("image.png", "PNG", "application/octet-stream"),
				after: reviewSide("image.png", "PNG2", "application/octet-stream"),
			},
		},
		{
			id: `${reviewRoot}/nul.dat`,
			edit: {
				before: reviewSide("nul.dat", "a"),
				after: reviewSide("nul.dat", "a\0b"),
			},
		},
		{
			id: `${reviewRoot}/utf8.txt`,
			edit: { after: reviewSide("utf8.txt", "€") },
		},
		{
			id: `${reviewRoot}/locked.txt`,
			edit: { after: reviewSide("locked.txt", "?") },
		},
	];
	const channel = workspaceChangeset();
	const { changesets } = await reviewChangesets(
		t,
		{ [channel]: { status: "ready", files } },
		{
			"ahp-content:/new%20file.ts:2": { data: "a\n" },
			"ahp-content:/gone.ts:1": { data: "x" },
			"ahp-content:/dir/from.ts:5": { data: "same\n" },
			"ahp-content:/dir/to.ts:10": { data: "same\nmore\n" },
			"ahp-content:/nul.dat:1": { data: "a" },
			"ahp-content:/nul.dat:3": { data: "a\0b" },
			"ahp-content:/utf8.txt:1": {
				data: Buffer.from("€\n").toString("base64"),
				encoding: "base64",
			},
			"ahp-content:/locked.txt:1": { error: -32603 },
		},
	);
	const diffs = await changesets.files(channel);
	assert.deepEqual(
		diffs.map((diff) => [diff.path, diff.original_path, diff.status]),
		[
			["new file.ts", undefined, "added"],
			["gone.ts", undefined, "deleted"],
			["dir/to.ts", "dir/from.ts", "modified"],
			["image.png", undefined, "modified"],
			["nul.dat", undefined, "modified"],
			["utf8.txt", undefined, "added"],
			["locked.txt", undefined, "added"],
		],
	);
	assert.match(
		diffs[0].patch,
		/^diff --git a\/new file.ts b\/new file.ts\n--- \/dev\/null\n\+\+\+ b\/new file.ts\n@@ -0,0 \+1 @@\n\+a\n$/,
	);
	assert.match(diffs[1].patch, /\n-x\n\\ No newline at end of file\n$/);
	assert.match(
		diffs[2].patch,
		/rename from dir\/from.ts\nrename to dir\/to.ts\n/,
	);
	assert.equal(diffs[2].original, "same\n");
	for (const index of [3, 4, 6]) {
		assert.match(
			diffs[index].patch,
			/\nBinary files .* differ\n$/,
			diffs[index].path,
		);
		assert.equal(diffs[index].original, undefined);
	}
	assert.equal(diffs[5].modified, "€\n");
	const one = await changesets.files(channel, "dir/to.ts", false);
	assert.equal(one.length, 1);
	assert.equal(one[0].modified, undefined, "summaries omit file contents");
});

test("a replaced content reference is retried once against the current snapshot", async (t) => {
	const channel = workspaceChangeset("unstaged");
	const { changesets, sent } = await reviewChangesets(
		t,
		{
			[channel]: {
				status: "ready",
				files: [
					{ id: `${reviewRoot}/a`, edit: { after: reviewSide("a", "x") } },
				],
			},
		},
		{},
	);
	await assert.rejects(
		changesets.files(channel),
		(error: any) => error.code === -32008,
	);
	assert.equal(sent("resourceRead").length, 2);
});

test("files outside the workspace are refused rather than shown under a wrong path", async (t) => {
	const channel = workspaceChangeset("staged");
	const { changesets } = await reviewChangesets(
		t,
		{
			[channel]: {
				status: "ready",
				files: [
					{
						id: "file:///work%20dir-evil/x",
						edit: {
							after: {
								...reviewSide("x", "x"),
								uri: "file:///work%20dir-evil/x",
							},
						},
					},
				],
			},
		},
		{ "ahp-content:/x:1": { data: "x" } },
	);
	await assert.rejects(changesets.files(channel), /outside the workspace/);
});

test("resource operations carry each file's nonce; stale requests are refused locally", async (t) => {
	const channel = workspaceChangeset("unstaged");
	const { changesets, sent } = await reviewChangesets(
		t,
		{
			[channel]: {
				status: "ready",
				files: [
					{
						id: `${reviewRoot}/a%20b.ts`,
						nonce: "n1",
						edit: { after: reviewSide("a%20b.ts", "x") },
					},
					{
						id: `${reviewRoot}/c.ts`,
						nonce: "n2",
						edit: { after: reviewSide("c.ts", "x") },
					},
				],
				operations: [
					{
						id: "stage",
						label: "Stage",
						scopes: ["changeset", "resource"],
						status: "idle",
					},
				],
			},
			[workspaceChangeset("staged")]: {
				status: "error",
				files: [],
				error: { errorType: "workspace", message: "no repository" },
			},
		},
		{},
	);
	await changesets.operate(channel, "stage", ["a b.ts"]);
	await changesets.operate(channel, "stage");
	assert.deepEqual(
		sent("invokeChangesetOperation").map((m) => m.params),
		[
			{
				channel,
				operationId: "stage",
				target: {
					kind: "resource",
					resource: `${reviewRoot}/a%20b.ts`,
					nonce: "n1",
				},
			},
			{ channel, operationId: "stage" },
		],
	);
	await assert.rejects(
		changesets.operate(channel, "discard-changes", ["c.ts"]),
		/no longer available/,
	);
	await assert.rejects(
		changesets.operate(channel, "stage", ["missing.ts"]),
		/missing.ts is no longer available/,
	);
	await assert.rejects(
		changesets.operate(workspaceChangeset("staged"), "unstage", ["c.ts"]),
		/no repository/,
	);
	assert.equal(sent("invokeChangesetOperation").length, 2);
});
