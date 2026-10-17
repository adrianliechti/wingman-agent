import assert from "node:assert/strict";
import test from "node:test";
import {
	SessionStore,
	emptySession,
	sessionKey,
	type SessionEvent,
} from "../src/state/sessionStore.ts";

test("stream bursts preserve unrelated views and summaries, and batch rendering", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	const store = new SessionStore();
	const first = sessionKey("wingman", "first"),
		second = sessionKey("wingman", "second");
	function snapshot(id: string): SessionEvent {
		return {
			type: "session.snapshot",
			subscriptionId: id,
			ref: { workspaceId: "w", backendId: "wingman", sessionId: id },
			epoch: "e",
			revision: 0,
			previousRevision: 0,
			state: {
				...emptySession(sessionKey("wingman", id)),
				status: "ready",
				phase: "streaming",
			},
			entries: [
				{ id: "user", type: "user", content: "task" },
				{ id: "reply", type: "assistant", content: "" },
			],
		};
	}
	store.expect(first, "first");
	store.apply(snapshot("first"));
	store.expect(second, "second");
	store.apply(snapshot("second"));
	const unrelated = store.getSnapshot()[second],
		metadata = store.getSummaries();
	let renders = 0;
	const release = store.subscribeRender(() => renders++);
	for (let revision = 1; revision <= 1000; revision++) {
		store.apply({
			...snapshot("first"),
			type: "session.update",
			revision,
			previousRevision: revision - 1,
			changes: [{ type: "entry.append", id: "reply", text: "x" }],
		});
	}
	assert.equal(store.getSnapshot()[first].entries[1].content.length, 1000);
	assert.equal(store.getSnapshot()[second], unrelated);
	assert.equal(store.getSummaries(), metadata);
	assert.equal(renders, 0);
	t.mock.timers.tick(16);
	assert.equal(renders, 1);
	release();
});
test("closed session histories are bounded without dropping live sessions", () => {
	const store = new SessionStore();
	for (let i = 0; i < 100; i++) {
		const id = `${i}`,
			key = sessionKey("wingman", id);
		store.expect(key, id);
		store.apply({
			type: "session.snapshot",
			subscriptionId: id,
			ref: { workspaceId: "w", backendId: "wingman", sessionId: id },
			epoch: "e",
			revision: 0,
			previousRevision: 0,
			state: {
				...emptySession(key),
				status: "ready",
				phase: i === 0 ? "streaming" : "idle",
			},
			entries: [],
		});
		store.forget(key);
	}
	assert.equal(Object.keys(store.getSnapshot()).length, 21);
	assert.equal(
		store.getSnapshot()[sessionKey("wingman", "0")].phase,
		"streaming",
	);
});
