import assert from "node:assert/strict";
import test from "node:test";
import type { SessionSummary } from "@microsoft/agent-host-protocol";
import type { AHPConnection } from "../src/state/ahpConnection.ts";
import { SessionCatalog } from "../src/state/sessionCatalog.ts";

const summary = (id: string, title = id): SessionSummary =>
	({
		resource: `ahp-session:/${id}`,
		provider: "wingman",
		title,
		status: 1,
		createdAt: "",
		modifiedAt: "",
	}) as SessionSummary;

/** A connection that answers `listSessions` when the test releases it. */
function fakeHost(items: SessionSummary[]) {
	let connected = true;
	const connection = new Set<() => void>();
	const notifications = new Set<(method: string, params: unknown) => void>();
	const requests: { method: string; params: any }[] = [];
	let release = () => {};
	let hold = false;
	let listError: Error | undefined;
	const ahp = {
		getConnected: () => connected,
		subscribeConnection: (fn: () => void) => {
			connection.add(fn);
			return () => connection.delete(fn);
		},
		onRootNotification: (fn: (method: string, params: unknown) => void) => {
			notifications.add(fn);
			return () => notifications.delete(fn);
		},
		request: async (method: string, params: any) => {
			requests.push({ method, params });
			if (method === "listSessions") {
				const listed = [...items];
				if (hold) await new Promise<void>((resolve) => (release = resolve));
				if (listError) throw listError;
				return { items: listed };
			}
			return {};
		},
	} as unknown as AHPConnection;
	return {
		ahp,
		items,
		requests,
		listed: () => requests.filter((r) => r.method === "listSessions").length,
		notify: (method: string, params: unknown) => {
			for (const fn of notifications) fn(method, params);
		},
		hold: () => {
			hold = true;
		},
		fail: () => {
			listError = new Error("listing failed");
		},
		release: () => {
			hold = false;
			release();
		},
		reconnect: () => {
			connected = false;
			for (const fn of connection) fn();
			connected = true;
			for (const fn of connection) fn();
		},
	};
}
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
/** Lets pending work run where a test holds the timers. */
const turn = () => new Promise((resolve) => setImmediate(resolve));
const titles = (catalog: SessionCatalog) =>
	catalog
		.getSnapshot()
		.sessions.map((session) => session.title)
		.sort();

test("a failed refresh preserves notifications received during the request", async () => {
	const host = fakeHost([summary("one")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	host.hold();
	const refresh = catalog.reload();
	host.notify("root/sessionRemoved", { session: "ahp-session:/one" });
	host.notify("root/sessionAdded", { summary: summary("two") });
	host.fail();
	host.release();
	await assert.rejects(refresh, /listing failed/);
	assert.deepEqual(titles(catalog), ["two"]);
});

test("the catalogue lists once and then follows the root's notifications", async () => {
	const host = fakeHost([summary("one"), summary("two")]);
	const seen: string[] = [];
	const catalog = new SessionCatalog(host.ahp, (s) => seen.push(s.resource));
	assert.equal(host.listed(), 0, "nothing is listed before anyone looks");
	let renders = 0;
	catalog.subscribe(() => renders++);
	await settle();
	assert.equal(catalog.getSnapshot().loaded, true);
	assert.deepEqual(titles(catalog), ["one", "two"]);
	assert.ok(seen.includes("ahp-session:/one"));

	host.notify("root/sessionAdded", { summary: summary("three") });
	host.notify("root/sessionRemoved", { session: "ahp-session:/one" });
	host.notify("root/sessionSummaryChanged", {
		session: "ahp-session:/two",
		changes: { title: "renamed", status: 9, activity: "Thinking" },
	});
	assert.deepEqual(titles(catalog), ["renamed", "three"]);
	const two = catalog
		.getSnapshot()
		.sessions.find((s) => s.resource === "ahp-session:/two")!;
	assert.equal(two.status, 9);
	assert.equal(two.activity, "Thinking");
	assert.equal(host.listed(), 1, "a busy session never lists everything again");
	assert.ok(renders >= 4);
});

test("notifications that arrive during a list apply on top of it", async () => {
	const host = fakeHost([summary("one")]);
	host.hold();
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	host.notify("root/sessionAdded", { summary: summary("late") });
	host.notify("root/sessionSummaryChanged", {
		session: "ahp-session:/one",
		changes: { title: "one, renamed" },
	});
	assert.equal(catalog.getSnapshot().loaded, false);
	host.release();
	await settle();
	assert.deepEqual(titles(catalog), ["late", "one, renamed"]);
});

test("an unknown session is listed again rather than guessed, and a reconnect relists", async (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	const host = fakeHost([summary("one")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	t.mock.timers.tick(0);
	await Promise.resolve();
	await Promise.resolve();
	assert.equal(host.listed(), 1);
	host.items.push(summary("other"));
	host.notify("root/sessionSummaryChanged", {
		session: "ahp-session:/other",
		changes: { title: "other" },
	});
	assert.deepEqual(titles(catalog), ["one"]);
	t.mock.timers.tick(250);
	await Promise.resolve();
	await Promise.resolve();
	assert.equal(host.listed(), 2);
	assert.deepEqual(titles(catalog), ["one", "other"]);
	host.reconnect();
	await Promise.resolve();
	await Promise.resolve();
	assert.equal(host.listed(), 3);
});

test("a reload asked for during a list gets a list that starts after it", async () => {
	const host = fakeHost([summary("one")]);
	host.hold();
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	host.items.push(summary("created meanwhile"));
	const again = catalog.reload();
	host.release();
	await again;
	assert.equal(host.listed(), 2);
	assert.deepEqual(titles(catalog), ["created meanwhile", "one"]);
});

test("a reconnect starts the included providers again", async () => {
	const host = fakeHost([summary("one")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	await catalog.include("codex");
	host.reconnect();
	await settle();
	await settle();
	const started = host.requests.filter(
		(r) => r.method === "resolveSessionConfig",
	);
	assert.deepEqual(
		started.map((r) => r.params.provider),
		["codex", "codex"],
	);
});

test("a provider that is still starting is reported until its sessions are listed", async (t) => {
	const host = fakeHost([summary("one")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	t.mock.timers.enable({ apis: ["setTimeout"] });
	host.hold();
	const included = catalog.include("codex");
	assert.deepEqual(catalog.getSnapshot().starting, ["codex"]);
	// Its sessions are listed a moment after it started.
	await turn();
	assert.equal(host.listed(), 1);
	t.mock.timers.tick(250);
	await turn();
	assert.equal(host.listed(), 2);
	assert.deepEqual(catalog.getSnapshot().starting, ["codex"]);
	host.release();
	await included;
	assert.deepEqual(catalog.getSnapshot().starting, []);
});

test("providers that start close together are listed together", async (t) => {
	const host = fakeHost([summary("one")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	t.mock.timers.enable({ apis: ["setTimeout"] });
	const included = Promise.all([
		catalog.include("codex"),
		catalog.include("claude"),
	]);
	await turn();
	assert.deepEqual(catalog.getSnapshot().starting, ["codex", "claude"]);
	t.mock.timers.tick(250);
	await included;
	assert.equal(host.listed(), 2, "one list for both");
	assert.deepEqual(catalog.getSnapshot().starting, []);
});

test("the list is a new array only when a session changed", async (t) => {
	const host = fakeHost([summary("one"), summary("two")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	t.mock.timers.enable({ apis: ["setTimeout"] });
	const listed = catalog.getSnapshot().sessions;
	const two = listed.find((s) => s.resource === "ahp-session:/two");
	// A provider that starts changes what the catalogue reports, not its sessions.
	void catalog.include("codex").catch(() => {});
	assert.deepEqual(catalog.getSnapshot().starting, ["codex"]);
	assert.equal(catalog.getSnapshot().sessions, listed);
	host.notify("root/sessionRemoved", { session: "ahp-session:/unknown" });
	assert.equal(catalog.getSnapshot().sessions, listed);
	host.notify("root/sessionSummaryChanged", {
		session: "ahp-session:/one",
		changes: { title: "renamed" },
	});
	assert.notEqual(catalog.getSnapshot().sessions, listed);
	assert.deepEqual(titles(catalog), ["renamed", "two"]);
	// A session that did not change keeps its summary.
	assert.ok(catalog.getSnapshot().sessions.includes(two!));
});

test("including a provider starts it once per connection and lists again", async () => {
	const host = fakeHost([summary("one")]);
	const catalog = new SessionCatalog(host.ahp);
	catalog.subscribe(() => {});
	await settle();
	await Promise.all([catalog.include("codex"), catalog.include("codex")]);
	const started = host.requests.filter(
		(r) => r.method === "resolveSessionConfig",
	);
	assert.deepEqual(
		started.map((r) => r.params.provider),
		["codex"],
	);
	// The host starts an agent that lists every folder only once for the list.
	assert.deepEqual(started[0].params._meta, { "wingman.listing": true });
	assert.equal(host.listed(), 2);
	catalog.forget("ahp-session:/one");
	assert.deepEqual(titles(catalog), []);
});
