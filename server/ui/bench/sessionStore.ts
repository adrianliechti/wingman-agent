// Run with node bench/sessionStore.ts. Optional argv[2] loads an older store
// implementation for a before/after comparison without changing this harness.
import { performance } from "node:perf_hooks";
const { SessionStore, emptySession, sessionKey } = await import(
	process.argv[2] ?? "../src/state/sessionStore.ts"
);
const store = new SessionStore();
const sessions = 50,
	entriesPerSession = 4000,
	deltas = 1000;
const template = Array.from({ length: entriesPerSession }, (_, i) => ({
	id: `${i}`,
	type: i % 2 ? "assistant" : "user",
	content: "Representative session content. ".repeat(4),
}));
for (let i = 0; i < sessions; i++) {
	const key = sessionKey("wingman", `${i}`);
	store.expect(key, `${i}`);
	store.apply({
		type: "session.snapshot",
		subscriptionId: `${i}`,
		ref: { workspaceId: "w", backendId: "wingman", sessionId: `${i}` },
		epoch: "e",
		revision: 0,
		previousRevision: 0,
		state: { ...emptySession(key), status: "ready", phase: "streaming" },
		entries: [...template],
	});
}
let notifications = 0;
const stop = (store.subscribeRender ?? store.subscribe)(() => {
	notifications++;
	if (!store.getSummaries)
		Object.fromEntries(
			Object.entries(store.getSnapshot()).map(([key, view]) => [
				key,
				{ ...(view as object) },
			]),
		);
});
const start = performance.now();
for (let revision = 1; revision <= deltas; revision++) {
	store.apply({
		type: "session.update",
		subscriptionId: "0",
		ref: { workspaceId: "w", backendId: "wingman", sessionId: "0" },
		epoch: "e",
		revision,
		previousRevision: revision - 1,
		changes: [
			{ type: "entry.append", id: `${entriesPerSession - 1}`, text: " token" },
		],
	});
}
const applyMs = performance.now() - start;
await new Promise((resolve) => setTimeout(resolve, 30));
stop();
console.log(
	JSON.stringify({
		sessions,
		entriesPerSession,
		deltas,
		applyMs: Math.round(applyMs * 10) / 10,
		renderNotifications: notifications,
	}),
);
