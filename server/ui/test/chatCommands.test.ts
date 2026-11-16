import assert from "node:assert/strict";
import test from "node:test";
import {
	completionCommand,
	sentAttachments,
	chatCommandsQuery,
} from "../src/api/chatCommands.ts";
import { ComposerDraft } from "../src/state/composerDraft.ts";
import { ComposerDrafts } from "../src/state/composerDrafts.ts";
import { draftChatTab } from "../src/mainLayout.ts";

const attachment = {
	type: "simple" as any,
	label: "/reports:shared",
	displayKind: "skill",
	_meta: {
		command: "reports:shared",
		isSkill: true,
		description: "Report",
		argumentHint: "[topic]",
		opaque: { preserve: [1, 2] },
	},
};

test("completion selection preserves opaque metadata and drops removed invocations", () => {
	const command = completionCommand({
		insertText: "/reports:shared",
		attachment,
	})!;
	assert.equal(command.kind, "skill");
	assert.equal(command.name, "reports:shared");
	assert.equal(command.attachment, attachment);
	for (const text of [
		"/reports:shared topic",
		"Use /reports:shared and continue",
	]) {
		assert.deepEqual(sentAttachments(text, [attachment]), [attachment]);
	}
	for (const text of [
		"no command",
		"https://example/reports:shared",
		"/reports:shared-extra",
		"/reports:shared/file",
	]) {
		assert.deepEqual(sentAttachments(text, [attachment]), []);
	}
});

test("attachments that are not commands are sent as they are", () => {
	const chat = {
		type: "chat" as any,
		label: "Earlier",
		resource: "ahp-chat:/c",
	};
	assert.deepEqual(sentAttachments("no command", [attachment, chat]), [chat]);
});

test("drafts query only the workspace catalogue without allocating a backend session", async (t) => {
	t.mock.method(globalThis, "fetch", async (url: string) => {
		assert.equal(url, "/api/workspace/skills");
		return Response.json([{ name: "reports:shared", description: "Report" }]);
	});
	const query = chatCommandsQuery(JSON.stringify(["not-started", ""]));
	assert.deepEqual(
		await query.queryFn!({ signal: new AbortController().signal } as never),
		[{ name: "reports:shared", description: "Report", kind: "skill" }],
	);
});

test("completion metadata survives saved drafts, queue editing and retry, then clears after acceptance", async () => {
	let stored: any[] = [];
	const storage = {
		read: async () => structuredClone(stored),
		write: async (value: any) => {
			stored = [structuredClone(value)];
		},
	};
	const tab = draftChatTab("wingman", "skills");
	const drafts = new ComposerDrafts(storage);
	const draft = drafts.get(tab);
	draft.update({ text: "/reports:shared topic", attachments: [attachment] });
	await drafts.flush();
	const reloaded = new ComposerDrafts(storage);
	await reloaded.load();
	const restored = reloaded.get(tab);
	assert.deepEqual(restored.getSnapshot().attachments, [attachment]);
	restored.beginQueueEdit({
		text: "another message",
		files: [],
		images: [],
		editingQueueId: "queued",
	});
	assert.equal(restored.getSnapshot().attachments, undefined);
	restored.cancelQueueEdit();
	assert.deepEqual(restored.getSnapshot().attachments, [attachment]);
	await restored.submit(() => false);
	assert.deepEqual(restored.getSnapshot().attachments, [attachment]);
	await restored.submit((content) => {
		assert.deepEqual(content.attachments, [attachment]);
		return true;
	});
	assert.equal(restored.getSnapshot().attachments, undefined);
	assert.deepEqual(restored.getSnapshot(), new ComposerDraft().getSnapshot());
});

test("a draft holding only a chat attachment is saved and restored", async () => {
	let stored: any[] = [];
	const storage = {
		read: async () => structuredClone(stored),
		write: async (value: any) => {
			stored = [structuredClone(value)];
		},
	};
	const tab = draftChatTab("wingman", "chat");
	const drafts = new ComposerDrafts(storage);
	const chat = {
		type: "chat" as any,
		label: "Earlier",
		resource: "ahp-chat:/c",
	};
	drafts.get(tab).update({ attachments: [chat] });
	await drafts.flush();
	drafts.syncTabs([]);
	await drafts.flush();
	const reloaded = new ComposerDrafts(storage);
	await reloaded.load();
	assert.deepEqual(
		reloaded.closedTabs().map((closed) => closed.tab.id),
		[tab.id],
	);
	assert.deepEqual(reloaded.get(tab).getSnapshot().attachments, [chat]);
});
