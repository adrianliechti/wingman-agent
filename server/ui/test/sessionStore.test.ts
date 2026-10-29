import assert from "node:assert/strict";
import test from "node:test";
import {
	SessionStore,
	emptySession,
	sessionKey,
} from "../src/state/sessionStore.ts";

test("presentation cache disconnects without losing history and accepts fresh epochs", () => {
	const store = new SessionStore(),
		key = sessionKey("one", "saved");
	store.expect(key);
	store.set(key, {
		...emptySession(key),
		status: "ready",
		epoch: "old",
		synchronized: true,
		entries: [{ id: "answer", type: "assistant", content: "retained" }],
	});
	store.disconnect();
	assert.equal(store.getSnapshot()[key].synchronized, false);
	assert.equal(store.getSnapshot()[key].entries[0].content, "retained");
	store.set(key, {
		...emptySession(key),
		status: "ready",
		epoch: "new",
		synchronized: true,
	});
	assert.equal(store.getSnapshot()[key].epoch, "new");
	assert.deepEqual(store.getSnapshot()[key].entries, []);
});
test("identical provider session ids remain scoped to their backend", () => {
	const store = new SessionStore(),
		first = sessionKey("one", "saved"),
		second = sessionKey("two", "saved");
	store.set(first, emptySession(first));
	store.set(second, {
		...emptySession(second),
		status: "error",
		error: "test",
	});
	assert.equal(store.getSnapshot()[first].status, "loading");
	assert.equal(store.getSnapshot()[second].error, "test");
});
