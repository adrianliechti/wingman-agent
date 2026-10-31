// Run with node bench/sessionStore.ts. Measures the actual SDK reducer,
// AHP projection and browser presentation store used for streaming updates.
import { performance } from "node:perf_hooks";
import { chatReducer } from "@microsoft/agent-host-protocol";
import { SessionStore } from "../src/state/sessionStore.ts";
import { AHPSessionProjection } from "../src/state/ahpSession.ts";
const store = new SessionStore();
const sessions = 50,
	entriesPerSession = 4000,
	deltas = 1000;
const turns = Array.from({ length: entriesPerSession / 2 }, (_, i) => ({
	id: `turn-${i}`,
	state: "complete",
	startedAt: "2026-01-01T00:00:00Z",
	duration: 0,
	message: { text: "Representative user message.", origin: { kind: "user" } },
	responseParts: [
		{
			kind: "markdown",
			id: `part-${i}`,
			content: "Representative assistant content. ".repeat(4),
		},
	],
}));
let active: any;
for (let i = 0; i < sessions; i++) {
	const key = `ahp-chat:/${i}`,
		projection = new AHPSessionProjection();
	const session: any = {
		provider: "wingman",
		title: "Benchmark",
		status: 1,
		lifecycle: "ready",
		activeClients: [],
		chats: [],
		defaultChat: key,
	};
	const chat: any = {
		resource: `ahp-chat:/${i}`,
		title: "Benchmark",
		status: 1,
		modifiedAt: "2026-01-01T00:00:00Z",
		turns: [...turns],
		activeTurn: {
			id: "active",
			message: { text: "Current prompt", origin: { kind: "user" } },
			startedAt: "2026-01-01T00:00:00Z",
			responseParts: [{ kind: "markdown", id: "answer", content: "" }],
		},
	};
	store.expect(key);
	store.set(key, projection.project(key, session, chat, 0));
	if (i === 0) active = { key, projection, session, chat };
}
let notifications = 0;
const stop = store.subscribeRender(() => notifications++);
const start = performance.now();
for (let revision = 1; revision <= deltas; revision++) {
	active.chat = chatReducer(active.chat, {
		type: "chat/delta",
		turnId: "active",
		partId: "answer",
		content: " token",
	} as any);
	store.set(
		active.key,
		active.projection.project(
			active.key,
			active.session,
			active.chat,
			revision,
		),
	);
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
