import assert from "node:assert/strict";
import test from "node:test";
import {
	SessionStore,
	canonicalBackend,
	canonicalChatURI,
	draftBackend,
	draftKey,
	emptySession,
	isDraft,
	migrateSessionKey,
} from "../src/state/sessionStore.ts";
import { migrateTab } from "../src/state/composerDrafts.ts";
import { chatTabId } from "../src/mainLayout.ts";

test("presentation cache disconnects without losing history and accepts fresh snapshots", () => {
	const store = new SessionStore(),
		key = "ahp-chat:/saved";
	store.expect(key);
	store.set(key, {
		...emptySession(key),
		status: "ready",
		revision: 1,
		synchronized: true,
		entries: [{ id: "answer", type: "assistant", content: "retained" }],
	});
	store.disconnect();
	assert.equal(store.getSnapshot()[key].synchronized, false);
	assert.equal(store.getSnapshot()[key].entries[0].content, "retained");
	store.set(key, {
		...emptySession(key),
		status: "ready",
		revision: 2,
		synchronized: true,
	});
	assert.equal(store.getSnapshot()[key].revision, 2);
	assert.deepEqual(store.getSnapshot()[key].entries, []);
});
test("chats are keyed by URI and drafts by their backend", () => {
	const store = new SessionStore(),
		first = "ahp-chat:/one",
		second = "ahp-chat:/two";
	store.set(first, emptySession(first));
	store.set(second, {
		...emptySession(second),
		status: "error",
		error: "test",
	});
	assert.equal(store.getSnapshot()[first].status, "loading");
	assert.equal(store.getSnapshot()[second].error, "test");
	assert.equal(isDraft(first), false);
	assert.equal(isDraft(draftKey("acp")), true);
	assert.equal(isDraft(""), true);
	assert.equal(draftBackend(draftKey("acp")), "acp");
	assert.equal(draftBackend(""), "wingman");
});
test("legacy session keys migrate to the canonical chat URI", () => {
	const uri = canonicalChatURI("claude-code", "sess/ü?");
	assert.equal(
		uri,
		`ahp-chat:/wingman/${Buffer.from("claude-code").toString("base64url")}/${Buffer.from("sess/ü?").toString("base64url")}`,
	);
	assert.doesNotMatch(uri, /[=+]/);
	assert.equal(canonicalBackend(uri), "claude-code");
	assert.equal(
		canonicalBackend(uri.replace("ahp-chat:", "ahp-session:")),
		"claude-code",
	);
	assert.equal(canonicalBackend("ahp-chat:/uuid"), undefined);
	assert.equal(
		migrateSessionKey(JSON.stringify(["claude-code", "sess/ü?"])),
		uri,
	);
	assert.equal(migrateSessionKey(uri), uri);
	assert.equal(migrateSessionKey("ahp-session:/x"), "ahp-chat:/x");
	assert.equal(migrateSessionKey(draftKey("wingman")), draftKey("wingman"));
	assert.equal(migrateSessionKey(""), "");
	assert.equal(migrateSessionKey("not json"), "not json");
});
test("saved chat tabs migrate their key and default tab id", () => {
	const legacy = JSON.stringify(["wingman", "saved"]);
	const uri = canonicalChatURI("wingman", "saved");
	assert.deepEqual(
		migrateTab({
			id: chatTabId(legacy),
			type: "chat",
			label: "Session",
			backendId: "wingman",
			sessionId: legacy,
		}),
		{
			id: chatTabId(uri),
			type: "chat",
			label: "Session",
			backendId: "wingman",
			sessionId: uri,
		},
	);
	const draft = {
		id: "draft:wingman:1",
		type: "chat" as const,
		label: "Agent",
		sessionId: legacy,
	};
	assert.equal(migrateTab(draft).id, "draft:wingman:1");
	assert.equal(migrateTab(draft).sessionId, uri);
	const current = { ...draft, sessionId: uri };
	assert.equal(migrateTab(current), current);
});
