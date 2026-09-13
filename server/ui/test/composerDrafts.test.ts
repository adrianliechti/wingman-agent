import assert from "node:assert/strict";
import test from "node:test";
import {
	ComposerDrafts,
	type DraftStorage,
	type SavedDraft,
} from "../src/state/composerDrafts.ts";
import { draftChatTab } from "../src/mainLayout.ts";

function memoryStorage(): DraftStorage {
	const saved = new Map<string, SavedDraft>();
	return {
		read: async () => structuredClone([...saved.values()]),
		write: async (value) => {
			saved.set(value.tab.id, structuredClone(value));
		},
	};
}
const content = {
	text: "Keep this work",
	files: ["main.go"],
	images: [{ id: "image", dataUrl: "data:image/png;base64,AA==" }],
	editingQueueId: "queue",
};
test("drafts recover text, files, images and queue edits on reload and after tab closure", async () => {
	const storage = memoryStorage();
	const tab = draftChatTab("wingman", "first");
	const drafts = new ComposerDrafts(storage);
	await drafts.load();
	drafts.get(tab).update(content);
	await drafts.flush();
	const reloaded = new ComposerDrafts(storage);
	await reloaded.load();
	assert.deepEqual(reloaded.openTabs(), [tab]);
	assert.deepEqual(reloaded.get(tab).getSnapshot(), {
		...content,
		submitting: false,
		error: null,
	});
	reloaded.syncTabs([]);
	await reloaded.flush();
	const closed = new ComposerDrafts(storage);
	await closed.load();
	assert.equal(closed.openTabs().length, 0);
	assert.equal(closed.closedTabs()[0].tab.id, tab.id);
	assert.equal(closed.get(tab).getSnapshot().text, content.text);
});
test("an older in-flight save cannot resurrect an accepted message", async () => {
	const storage = memoryStorage();
	const firstWrite = Promise.withResolvers<void>();
	let writes = 0;
	const drafts = new ComposerDrafts({
		...storage,
		write: async (value) => {
			if (!writes++) await firstWrite.promise;
			await storage.write(value);
		},
	});
	const tab = draftChatTab("wingman", "first");
	const draft = drafts.get(tab);
	draft.update(content);
	await draft.submit(() => true);
	firstWrite.resolve();
	assert.equal(await drafts.flush(), true);
	const restored = new ComposerDrafts(storage);
	await restored.load();
	assert.deepEqual(restored.openTabs(), []);
	assert.equal(restored.get(tab).getSnapshot().text, "");
});
test("storage failure retains the draft, protects window close, and can be retried", async () => {
	const storage = memoryStorage();
	let fail = true;
	const drafts = new ComposerDrafts({
		...storage,
		write: async (value) => {
			if (fail) throw new Error("quota");
			await storage.write(value);
		},
	});
	const tab = draftChatTab("wingman", "first");
	drafts.get(tab).update(content);
	assert.equal(await drafts.flush(), false);
	assert.equal(drafts.needsProtection(), true);
	assert.ok(drafts.getError());
	assert.equal(drafts.get(tab).getSnapshot().images.length, 1);
	fail = false;
	assert.equal(await drafts.flush(), true);
	assert.equal(drafts.needsProtection(), false);
	assert.equal(drafts.getError(), null);
});
test("session association survives promotion and reopening under another tab identity", async () => {
	const storage = memoryStorage();
	const drafts = new ComposerDrafts(storage);
	const tab = draftChatTab("wingman", "first");
	drafts.get(tab).update(content);
	const session = { ...tab, sessionId: '["wingman","saved"]' };
	drafts.syncTabs([session]);
	drafts.syncTabs([]);
	await drafts.flush();
	const reopened = { ...session, id: "chat:saved" };
	const draft = drafts.get(reopened);
	assert.equal(draft.getSnapshot().text, content.text);
	await draft.submit(() => true);
	await drafts.flush();
	const restored = new ComposerDrafts(storage);
	await restored.load();
	assert.deepEqual(restored.closedTabs(), []);
	assert.deepEqual(restored.openTabs(), []);
});

test("recovering a closed draft persists its new identity before removing the old one", async () => {
	const storage = memoryStorage();
	const drafts = new ComposerDrafts(storage);
	const tab = {
		...draftChatTab("wingman", "first"),
		sessionId: '["wingman","saved"]',
	};
	drafts.get(tab).update(content);
	drafts.syncTabs([]);
	await drafts.flush();
	const reopened = { ...tab, id: "chat:saved" };
	drafts.get(reopened);
	await drafts.flush();
	const restored = new ComposerDrafts(storage);
	await restored.load();
	assert.equal(restored.openTabs()[0].id, reopened.id);
	assert.equal(restored.get(reopened).getSnapshot().text, content.text);
	assert.deepEqual(restored.closedTabs(), []);
});

test("queue editing persists both the edit and the separate draft", async () => {
	const storage = memoryStorage(),
		registry = new ComposerDrafts(storage);
	const tab = draftChatTab("wingman", "first"),
		draft = registry.get(tab);
	draft.update(content);
	await registry.flush();
	draft.beginQueueEdit({
		text: "queued",
		files: [],
		images: [],
		editingQueueId: "q",
	});
	draft.update({ text: "updated queue" });
	assert.equal(await registry.flush(), true);
	const recovered = new ComposerDrafts(storage);
	await recovered.load();
	assert.equal(recovered.get(tab).getSnapshot().text, "updated queue");
	recovered.get(tab).cancelQueueEdit();
	assert.equal(recovered.get(tab).getSnapshot().text, content.text);
});

test("navigation journals edits waiting behind an unfinished IndexedDB write", async () => {
	const storage = memoryStorage(),
		gate = Promise.withResolvers<void>();
	let journal: import("../src/state/composerDrafts.ts").SavedDraft[] = [];
	const registry = new ComposerDrafts({
		...storage,
		write: async (value) => {
			await gate.promise;
			await storage.write(value);
		},
		protect: (pending) => {
			journal = structuredClone(pending);
		},
	});
	const tab = draftChatTab("wingman", "first"),
		draft = registry.get(tab);
	draft.update(content);
	draft.beginQueueEdit({
		text: "queue",
		files: [],
		images: [],
		editingQueueId: "q",
	});
	draft.update({ text: "latest queue edit" });
	assert.equal(registry.protectPending(), true);
	const recovered = new ComposerDrafts({
		...storage,
		recover: () => journal,
		clearRecovery: () => {
			journal = [];
		},
	});
	await recovered.load();
	await recovered.flush();
	assert.equal(recovered.get(tab).getSnapshot().text, "latest queue edit");
	recovered.get(tab).cancelQueueEdit();
	assert.equal(recovered.get(tab).getSnapshot().text, content.text);
	assert.deepEqual(journal, []);
	gate.resolve();
	await registry.flush();
});

test("navigation recovery still works when IndexedDB cannot be read", async () => {
	const tab = draftChatTab("wingman", "journal");
	const registry = new ComposerDrafts({
		read: async () => {
			throw new Error("IndexedDB unavailable");
		},
		write: async () => {
			throw new Error("IndexedDB unavailable");
		},
		recover: () => [{ tab, content, open: true, updated: 1 }],
	});
	await registry.load();
	assert.deepEqual(registry.openTabs(), [tab]);
	assert.equal(registry.get(tab).getSnapshot().text, content.text);
	assert.equal(await registry.flush(), false);
	assert.ok(registry.getError());
});

test("one malformed draft does not block recovery of the remaining drafts", async () => {
	const tab = draftChatTab("wingman", "valid");
	for (const malformed of [
		null,
		{
			tab: { ...tab, id: "broken" },
			content: { ...content, previousDraft: {} },
			updated: 1,
			open: true,
		},
	]) {
		const registry = new ComposerDrafts({
			...memoryStorage(),
			read: async () =>
				[malformed, { tab, content, open: true, updated: 2 }] as SavedDraft[],
		});
		await registry.load();
		assert.deepEqual(registry.openTabs(), [tab]);
		assert.equal(registry.get(tab).getSnapshot().text, content.text);
	}
});

test("a retired identity cannot be revived by an older navigation journal", async () => {
	const storage = memoryStorage();
	const tab = {
		...draftChatTab("wingman", "closed"),
		sessionId: '["wingman","saved"]',
	};
	const registry = new ComposerDrafts(storage);
	registry.get(tab).update(content);
	registry.syncTabs([]);
	await registry.flush();
	const journal = await storage.read();
	const reopened = { ...tab, id: "chat:saved" };
	await registry.get(reopened).submit(() => true);
	await registry.flush();
	const reloaded = new ComposerDrafts({ ...storage, recover: () => journal });
	await reloaded.load();
	assert.deepEqual(reloaded.closedTabs(), []);
	assert.deepEqual(reloaded.openTabs(), []);
});
