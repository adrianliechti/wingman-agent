import assert from "node:assert/strict";
import test from "node:test";
import type { SessionSummary } from "@microsoft/agent-host-protocol";
import { SessionStatus } from "../src/state/ahpValues.ts";
import {
	attentionQueue,
	fleetCounts,
	fleetEvents,
	fleetSession,
	fleetSessions,
	fleetState,
	FleetViewStore,
	groupFleet,
	newestSessions,
	nextAttention,
	relativeTime,
	restoreFleetView,
	storeFleetView,
	DEFAULT_FLEET_VIEW,
	type FleetState,
} from "../src/state/fleet.ts";

const {
	Idle: IDLE,
	Error: ERROR,
	InProgress: IN_PROGRESS,
	InputNeeded: INPUT_NEEDED,
	IsRead: READ,
	IsArchived: ARCHIVED,
} = SessionStatus;
const now = new Date("2026-11-17T12:00:00Z");

function summary(
	id: string,
	status: number,
	modifiedAt = "2026-11-17T11:00:00Z",
	extra: Partial<SessionSummary> = {},
): SessionSummary {
	return {
		resource: `ahp-session:/${id}`,
		provider: "wingman",
		title: id,
		status,
		createdAt: modifiedAt,
		modifiedAt,
		defaultChat: `ahp-chat:/${id}`,
		...extra,
	} as SessionSummary;
}

test("status bits map to what a session does", () => {
	assert.equal(fleetState(IDLE | READ), "idle");
	assert.equal(fleetState(IN_PROGRESS), "working");
	assert.equal(fleetState(INPUT_NEEDED), "needs-input");
	assert.equal(fleetState(ERROR), "failed");
	assert.equal(fleetState(INPUT_NEEDED | ERROR), "needs-input");
});

test("a session row takes its default chat's flags and lists its forks", () => {
	const session = fleetSession(
		summary("one", IN_PROGRESS | READ, undefined, {
			activity: "Run command: go test",
			changes: { additions: 4, deletions: 1, files: 2 },
			chats: [
				{ resource: "ahp-chat:/one", title: "", status: IDLE },
				{ resource: "ahp-chat:/fork", title: "", status: INPUT_NEEDED },
				{ resource: "ahp-chat:/saved-fork", title: "Saved" },
			],
		}),
	);
	assert.equal(session.id, "ahp-chat:/one");
	// The default chat rests and is unread; a fork that waits speaks for the session.
	assert.equal(session.state, "idle");
	assert.equal(session.unread, true);
	assert.equal(session.sessionState, "needs-input");
	assert.equal(session.activity, "Run command: go test");
	assert.deepEqual(session.changes, { additions: 4, deletions: 1, files: 2 });
	assert.deepEqual(
		session.forks.map((fork) => [fork.id, fork.title, fork.state, fork.unread]),
		[
			["ahp-chat:/fork", "Fork of one", "needs-input", true],
			// A chat the host lists without status is not new.
			["ahp-chat:/saved-fork", "Saved", "idle", false],
		],
	);
	assert.equal(
		fleetSession(summary("plain", IDLE | READ)).unread,
		false,
		"without chat flags the session's are used",
	);
});

test("a running fork keeps its session working until it finishes", () => {
	const forkRunning = (status: number) =>
		fleetSession(
			summary("one", IDLE | READ, undefined, {
				chats: [
					{ resource: "ahp-chat:/one", title: "", status: IDLE | READ },
					{ resource: "ahp-chat:/fork", title: "", status },
				],
			}),
		);
	const running = forkRunning(IN_PROGRESS);
	assert.equal(running.state, "idle");
	assert.equal(running.sessionState, "working");
	assert.deepEqual(fleetCounts([running]), { needsInput: 0, working: 1 });
	assert.deepEqual(attentionQueue([running]), []);

	const finished = forkRunning(IDLE);
	assert.deepEqual(attentionQueue([finished]), ["ahp-chat:/fork"]);
	assert.deepEqual(
		fleetEvents(new Map([[running.session, running.sessionState]]), [
			finished,
		]).map((event) => event.kind),
		["finished"],
	);
});

test("the list shows what needs the user, then what works, then the rest by day", () => {
	const sessions = [
		summary("old", IDLE | READ, "2026-11-01T10:00:00Z"),
		summary("waiting", INPUT_NEEDED | READ, "2026-11-17T09:00:00Z"),
		summary(
			"archived-waiting",
			INPUT_NEEDED | ARCHIVED,
			"2026-11-10T09:00:00Z",
		),
		summary("working", IN_PROGRESS, "2026-11-17T11:30:00Z"),
		summary("archived-working", IN_PROGRESS | ARCHIVED, "2026-11-17T09:00:00Z"),
		summary("archived", IDLE | READ | ARCHIVED, "2026-11-17T08:00:00Z"),
		summary("today", IDLE, "2026-11-17T10:00:00Z"),
		summary("yesterday", ERROR, "2026-11-16T10:00:00Z"),
	].map(fleetSession);
	const view = (
		filter: Parameters<typeof groupFleet>[1]["filter"],
		search = "",
	) =>
		groupFleet(sessions, { filter, search, now }).map((group) => [
			group.label,
			group.sessions.map((session) => session.title),
		]);
	assert.deepEqual(view("all"), [
		["Needs you", ["waiting", "archived-waiting"]],
		["Working", ["working"]],
		["Today", ["today"]],
		["Yesterday", ["yesterday"]],
		["Older", ["old"]],
	]);
	assert.deepEqual(view("needs-input"), [
		["Needs you", ["waiting", "archived-waiting"]],
	]);
	assert.deepEqual(view("running"), [["Working", ["working"]]]);
	assert.deepEqual(view("archived"), [
		["Today", ["archived-working", "archived"]],
		["This week", ["archived-waiting"]],
	]);
	assert.deepEqual(view("all", "YESTER"), [["Yesterday", ["yesterday"]]]);
	assert.deepEqual(fleetCounts(sessions), { needsInput: 2, working: 1 });
	assert.deepEqual(
		groupFleet(
			[
				fleetSession(
					summary("codex", IDLE | READ, undefined, { provider: "codex" }),
				),
			],
			{ filter: "all", provider: "wingman", now },
		),
		[],
	);
	assert.deepEqual(
		groupFleet(
			[
				fleetSession(
					summary("x", IDLE | READ, undefined, { provider: "codex" }),
				),
			],
			{
				filter: "all",
				search: "codex cli",
				providerName: () => "Codex CLI",
				now,
			},
		).length,
		1,
	);
});

test("the sessions of several projects group by project, the window's first", () => {
	const project = (name: string) => ({
		uri: `file:///work/${name}`,
		displayName: name,
	});
	const sessions = [
		summary("api-new", IDLE | READ, "2026-11-17T11:00:00Z", {
			project: project("api"),
		}),
		summary("here-old", IDLE | READ, "2026-11-10T11:00:00Z", {
			project: project("here"),
		}),
		// The host may spell the window's directory without its links resolved.
		summary("here-linked", IDLE, "2026-11-17T10:00:00Z", {
			project: { uri: "file:///link/here", displayName: "here" },
		}),
		summary("web", IDLE | READ, "2026-11-16T11:00:00Z", {
			project: project("web"),
		}),
		summary("api-waits", INPUT_NEEDED, "2026-11-17T09:00:00Z", {
			project: project("api"),
		}),
	].map(fleetSession);
	const here = (session: (typeof sessions)[number]) =>
		["file:///work/here", "file:///link/here"].includes(
			session.project?.uri ?? "",
		);
	const groups = groupFleet(sessions, { filter: "all", here, now });
	assert.deepEqual(
		groups.map((group) => [
			group.label,
			group.project?.here,
			group.sessions.map((session) => session.title),
		]),
		[
			["Needs you", undefined, ["api-waits"]],
			["here", true, ["here-linked", "here-old"]],
			["api", false, ["api-new"]],
			["web", false, ["web"]],
		],
	);
	// A project's group is named by its URI, the window's own by the window's:
	// the same project is the same group in whichever window lists it.
	const ids = (ownProject?: string) =>
		groupFleet(sessions, { filter: "all", here, ownProject, now })
			.filter((group) => group.project)
			.map((group) => group.id);
	assert.deepEqual(ids("file:///work/here"), [
		"project:file:///work/here",
		"project:file:///work/api",
		"project:file:///work/web",
	]);
	// A window whose host names no project keeps the bare key for its own.
	assert.equal(ids()[0], "project:");
	// One project's sessions keep their days.
	assert.deepEqual(
		groupFleet(sessions.filter(here), { filter: "all", here, now }).map(
			(group) => group.label,
		),
		["Today", "This week"],
	);
});

test("a list derives its sessions once, and an unchanged summary keeps its session", () => {
	const one = summary("one", IDLE | READ, "2026-11-17T09:00:00Z");
	const two = summary("two", IN_PROGRESS, "2026-11-17T10:00:00Z");
	const list = [one, two];
	const sessions = fleetSessions(list);
	assert.equal(fleetSessions(list), sessions);
	assert.equal(fleetCounts(sessions), fleetCounts(sessions));
	assert.equal(newestSessions(sessions), newestSessions(sessions));
	assert.deepEqual(
		newestSessions(sessions).map((session) => session.title),
		["two", "one"],
	);
	assert.deepEqual(
		sessions.map((session) => session.title),
		["one", "two"],
		"the list keeps the host's order",
	);
	// The host replaces the summary of a session that changes.
	const next = fleetSessions([one, { ...two, status: INPUT_NEEDED }]);
	assert.equal(next[0], sessions[0]);
	assert.notEqual(next[1], sessions[1]);
	assert.deepEqual(fleetCounts(next), { needsInput: 1, working: 0 });
	assert.deepEqual(fleetCounts(sessions), { needsInput: 0, working: 1 });
	// A session without a date sorts last.
	assert.deepEqual(
		newestSessions(
			[summary("undated", IDLE, ""), summary("dated", IDLE)].map(fleetSession),
		).map((session) => session.title),
		["dated", "undated"],
	);
});

test("next visits waiting chats, then unseen ones, and wraps around", () => {
	const sessions = [
		summary("seen", IDLE | READ, "2026-11-17T11:50:00Z"),
		summary("unseen", IDLE, "2026-11-17T11:40:00Z"),
		summary("failed", ERROR, "2026-11-17T11:30:00Z"),
		summary("working", IN_PROGRESS, "2026-11-17T11:59:00Z"),
		summary("archived", IDLE | ARCHIVED, "2026-11-17T11:58:00Z"),
		summary("fork-waits", INPUT_NEEDED | READ, "2026-11-17T11:00:00Z", {
			chats: [
				{ resource: "ahp-chat:/fork-waits", title: "", status: IDLE | READ },
				{ resource: "ahp-chat:/its-fork", title: "", status: INPUT_NEEDED },
			],
		}),
		summary("waits", INPUT_NEEDED, "2026-11-17T10:00:00Z"),
	].map(fleetSession);
	assert.deepEqual(attentionQueue(sessions), [
		"ahp-chat:/its-fork",
		"ahp-chat:/waits",
		"ahp-chat:/unseen",
		"ahp-chat:/failed",
	]);
	assert.equal(nextAttention(sessions, "ahp-chat:/seen"), "ahp-chat:/its-fork");
	assert.equal(nextAttention(sessions, "ahp-chat:/waits"), "ahp-chat:/unseen");
	assert.equal(
		nextAttention(sessions, "ahp-chat:/failed"),
		"ahp-chat:/its-fork",
	);
	assert.equal(
		nextAttention([fleetSession(summary("only", IDLE))], "ahp-chat:/only"),
		undefined,
		"the only chat that wants the user is already open",
	);
	assert.equal(nextAttention([], ""), undefined);
});

test("events report what began to wait, finished or failed", () => {
	const before = new Map<string, FleetState>([
		["ahp-session:/a", "working"],
		["ahp-session:/b", "working"],
		["ahp-session:/c", "idle"],
		["ahp-session:/d", "needs-input"],
		["ahp-session:/e", "idle"],
	]);
	const sessions = [
		summary("a", IDLE),
		summary("b", ERROR),
		summary("c", INPUT_NEEDED),
		summary("d", IN_PROGRESS),
		summary("e", IN_PROGRESS),
		summary("new", INPUT_NEEDED),
	].map(fleetSession);
	assert.deepEqual(
		fleetEvents(before, sessions).map((event) => [
			event.session.title,
			event.kind,
		]),
		[
			["a", "finished"],
			["b", "failed"],
			["c", "needs-input"],
		],
	);
});

test("relative times stay short", () => {
	assert.equal(relativeTime("2026-11-17T11:59:40Z", now), "now");
	assert.equal(relativeTime("2026-11-17T11:15:00Z", now), "45m");
	assert.equal(relativeTime("2026-11-17T03:00:00Z", now), "9h");
	assert.equal(relativeTime("2026-11-14T12:00:00Z", now), "3d");
	assert.equal(relativeTime("", now), "");
	assert.equal(relativeTime("not a date", now), "");
});

test("the list's view follows the window to another project", () => {
	const a = "file:///work/a";
	const b = "file:///work/b";
	const c = "file:///work/c";
	// In project a: its own group open as it starts, b opened, c closed again.
	const stored = storeFleetView(
		{
			filter: "needs-input",
			provider: "codex",
			search: "",
			searching: false,
			expanded: { [`project:${b}`]: true, [`project:${c}`]: false },
		},
		a,
	);
	// In project b the list is as the window left it, and a stays open.
	assert.deepEqual(restoreFleetView(stored), {
		filter: "needs-input",
		provider: "codex",
		search: "",
		searching: false,
		expanded: {
			[`project:${a}`]: true,
			[`project:${b}`]: true,
			[`project:${c}`]: false,
		},
	});
	// A group the user closed in its own window stays closed from elsewhere.
	const closed = storeFleetView(
		{ ...DEFAULT_FLEET_VIEW, expanded: { [`project:${a}`]: false } },
		a,
	);
	assert.deepEqual(restoreFleetView(closed).expanded, {
		[`project:${a}`]: false,
	});
});

test("a search travels with its field open, and a damaged view starts afresh", () => {
	const stored = storeFleetView({ ...DEFAULT_FLEET_VIEW, search: "deploy" });
	assert.deepEqual(restoreFleetView(stored), {
		...DEFAULT_FLEET_VIEW,
		search: "deploy",
		searching: true,
	});
	for (const damaged of [null, "", "{", "null", "7", '"text"'])
		assert.deepEqual(restoreFleetView(damaged), DEFAULT_FLEET_VIEW);
	assert.deepEqual(
		restoreFleetView(
			'{"filter":"bogus","provider":3,"expanded":{"project:file:///x":"yes"}}',
		),
		DEFAULT_FLEET_VIEW,
	);
});

/** A window's storage, which the page that follows reads again. */
function windowStorage(works = true) {
	let stored: string | null = null;
	return {
		read: () => stored,
		write: (view: string) => {
			if (works) stored = view;
		},
	};
}

test("the view store shares one view and keeps it for the window's next page", () => {
	const a = "file:///work/a";
	const b = "file:///work/b";
	const storage = windowStorage();
	const store = new FleetViewStore(storage, a);
	assert.deepEqual(store.getSnapshot(), DEFAULT_FLEET_VIEW);
	let changes = 0;
	const release = store.subscribe(() => changes++);
	store.set({ filter: "running" });
	store.set({ expanded: { [`project:${b}`]: true } });
	assert.equal(changes, 2);
	assert.equal(store.getSnapshot().filter, "running");
	assert.equal(store.getSnapshot(), store.getSnapshot());
	release();
	store.set({ search: "deploy" });
	assert.equal(changes, 2);
	// The page of project b reads the view that the page of a left.
	assert.deepEqual(new FleetViewStore(storage, b).getSnapshot(), {
		filter: "running",
		provider: "",
		search: "deploy",
		searching: true,
		expanded: { [`project:${a}`]: true, [`project:${b}`]: true },
	});
});

test("a window that leaves its project keeps it open in the list, with or without storage", () => {
	const a = "file:///work/a";
	const storage = windowStorage();
	// A list the user never touched is kept as the window moves on.
	new FleetViewStore(storage, a).keep();
	assert.deepEqual(new FleetViewStore(storage).getSnapshot().expanded, {
		[`project:${a}`]: true,
	});
	// A host that names no project has no group to record.
	const unnamed = windowStorage();
	new FleetViewStore(unnamed).keep();
	assert.deepEqual(
		new FleetViewStore(unnamed).getSnapshot(),
		DEFAULT_FLEET_VIEW,
	);
	// Where the browser keeps nothing the view still holds for the page.
	const none = windowStorage(false);
	const store = new FleetViewStore(none, a);
	store.set({ filter: "archived" });
	assert.equal(store.getSnapshot().filter, "archived");
	assert.deepEqual(
		new FleetViewStore(none, a).getSnapshot(),
		DEFAULT_FLEET_VIEW,
	);
});
