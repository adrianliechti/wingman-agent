import assert from "node:assert/strict";
import test from "node:test";
import { chatReducer } from "@microsoft/agent-host-protocol";
import {
	SessionStore,
	emptySession,
	sessionKey,
} from "../src/state/sessionStore.ts";
import { AHPSessionProjection } from "../src/state/ahpSession.ts";

test("AHP token bursts retain completed entries and unrelated summaries and batch renders", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	const store = new SessionStore(),
		projection = new AHPSessionProjection();
	const key = sessionKey("one", "first"),
		other = sessionKey("one", "other");
	const session: any = {
		_meta: {
			wingman: {
				...emptySession(key),
				ref: { backendId: "one", sessionId: "first", workspaceId: "w" },
				epoch: "e",
				phase: "streaming",
				status: "ready",
			},
		},
	};
	let chat: any = {
		resource: "ahp-chat:/first",
		title: "Test",
		status: 1,
		modifiedAt: "2026-01-01T00:00:00Z",
		turns: [
			{
				id: "old",
				message: { text: "old", origin: { kind: "user" } },
				responseParts: [
					{ kind: "markdown", id: "old-answer", content: "completed" },
				],
				state: "complete",
			},
		],
		activeTurn: {
			id: "active",
			startedAt: "2026-01-01T00:00:00Z",
			message: { text: "new", origin: { kind: "user" } },
			responseParts: [{ kind: "markdown", id: "answer", content: "" }],
		},
	};
	store.set(key, projection.project(key, session, chat, 0));
	store.set(other, emptySession(other));
	const completed = store.getSnapshot()[key].entries[1],
		unrelated = store.getSnapshot()[other],
		summaries = store.getSummaries();
	let renders = 0;
	const release = store.subscribeRender(() => renders++);
	for (let revision = 1; revision <= 1000; revision++) {
		chat = chatReducer(chat, {
			type: "chat/delta",
			turnId: "active",
			partId: "answer",
			content: "x",
		} as any);
		store.set(key, projection.project(key, session, chat, revision));
	}
	assert.equal(store.getSnapshot()[key].entries.at(-1)?.content.length, 1000);
	assert.equal(store.getSnapshot()[key].entries[1], completed);
	assert.equal(store.getSnapshot()[other], unrelated);
	assert.equal(store.getSummaries(), summaries);
	assert.equal(renders, 0);
	t.mock.timers.tick(16);
	assert.equal(renders, 1);
	release();
});
test("closed histories are bounded without discarding an active session", () => {
	const store = new SessionStore();
	for (let i = 0; i < 100; i++) {
		const key = sessionKey("one", String(i));
		store.expect(key);
		store.set(key, {
			...emptySession(key),
			phase: i === 0 ? "streaming" : "idle",
		});
		store.forget(key);
	}
	assert.equal(Object.keys(store.getSnapshot()).length, 21);
	assert.equal(store.getSnapshot()[sessionKey("one", "0")].phase, "streaming");
});
