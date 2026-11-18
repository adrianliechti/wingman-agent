import type { SessionSummary } from "@microsoft/agent-host-protocol";
import { sessionToChat } from "./sessionStore.ts";

// SessionStatus bits; the SDK declares them as a const enum.
const ERROR = 1 << 1;
const IN_PROGRESS = 1 << 3;
const INPUT_NEEDED = 1 << 4; // InputNeeded (24) also sets InProgress
const IS_READ = 1 << 5;
const IS_ARCHIVED = 1 << 6;

/** What a session or chat does: wait for the user, work, or rest after a failure or success. */
export type FleetState = "needs-input" | "working" | "failed" | "idle";
export type FleetChanges = {
	additions: number;
	deletions: number;
	files: number;
};
export interface FleetChat {
	/** The chat URI that a tab opens. */
	id: string;
	title: string;
	state: FleetState;
	/** Holds a turn no client has looked at. Unknown read state is read. */
	unread: boolean;
	changes?: FleetChanges;
}
export interface FleetSession extends FleetChat {
	/** The state of the whole session: a chat that waits, works or fails speaks for it. */
	sessionState: FleetState;
	session: string;
	provider: string;
	activity?: string;
	archived: boolean;
	modifiedAt: string;
	project?: { uri: string; displayName: string };
	forks: FleetChat[];
}

export function fleetState(status = 1): FleetState {
	if (status & INPUT_NEEDED) return "needs-input";
	if (status & IN_PROGRESS) return "working";
	if (status & ERROR) return "failed";
	return "idle";
}

const urgency: readonly FleetState[] = [
	"needs-input",
	"working",
	"failed",
	"idle",
];

function mostUrgent(states: readonly FleetState[]): FleetState {
	return urgency.find((state) => states.includes(state)) ?? "idle";
}

function changesOf(summary?: {
	additions?: number;
	deletions?: number;
	files?: number;
}): FleetChanges | undefined {
	if (!summary) return;
	return {
		additions: summary.additions ?? 0,
		deletions: summary.deletions ?? 0,
		files: summary.files ?? 0,
	};
}

export function fleetSession(summary: SessionSummary): FleetSession {
	const defaultChat = summary.defaultChat ?? sessionToChat(summary.resource);
	const chats = summary.chats ?? [];
	const own = chats.find((chat) => chat.resource === defaultChat);
	// The default chat's own flags, when the host listed them, else the session's.
	const status = own?.status ?? summary.status;
	const title = summary.title || "Untitled session";
	const forks = chats
		.filter((chat) => chat.resource !== defaultChat)
		.map((chat) => ({
			id: chat.resource,
			title: chat.title || `Fork of ${title}`,
			state: fleetState(chat.status),
			unread: chat.status !== undefined && (chat.status & IS_READ) === 0,
			changes: changesOf(chat.changes),
		}));
	return {
		id: defaultChat,
		session: summary.resource,
		title,
		provider: summary.provider,
		state: fleetState(status),
		// The host reports the default chat's work only; a running fork keeps
		// the session working, so it finishes, and notifies, with its last chat.
		sessionState: mostUrgent([
			fleetState(summary.status),
			fleetState(status),
			...forks.map((fork) => fork.state),
		]),
		unread: (status & IS_READ) === 0,
		archived: (summary.status & IS_ARCHIVED) !== 0,
		activity: summary.activity || undefined,
		modifiedAt: summary.modifiedAt,
		project: summary.project,
		changes: changesOf(summary.changes ?? own?.changes),
		forks,
	};
}

export type FleetFilter = "all" | "needs-input" | "running" | "archived";
const FLEET_FILTERS: readonly FleetFilter[] = [
	"all",
	"needs-input",
	"running",
	"archived",
];
/** What a session list shows of the sessions: its filter, its search, and the
 * project groups the user opened or closed, by group id. */
export type FleetView = {
	filter: FleetFilter;
	provider: string;
	search: string;
	searching: boolean;
	expanded: Readonly<Record<string, boolean>>;
};
export const DEFAULT_FLEET_VIEW: FleetView = {
	filter: "all",
	provider: "",
	search: "",
	searching: false,
	expanded: {},
};
/** The group of the window's own project, whichever project that is. */
const HERE_GROUP = "project:";

/** A list's view as a window that moves to another project finds it again.
 * Groups are kept by their project, since the window's own changes: the
 * project it leaves stays open in the list, as it was while the window
 * showed it. */
export function storeFleetView(view: FleetView, here?: string): string {
	const groups: Record<string, boolean> = {};
	for (const [id, open] of Object.entries(view.expanded)) {
		if (id !== HERE_GROUP) groups[id.slice(HERE_GROUP.length)] = open;
	}
	if (here) groups[here] = view.expanded[HERE_GROUP] ?? true;
	return JSON.stringify({ ...view, expanded: groups });
}

/** Reads a stored view in the window of the project `here`. */
export function restoreFleetView(
	stored: string | null,
	here?: string,
): FleetView {
	let saved: Partial<Record<keyof FleetView, unknown>>;
	try {
		saved = JSON.parse(stored ?? "") ?? {};
	} catch {
		return DEFAULT_FLEET_VIEW;
	}
	if (typeof saved !== "object") return DEFAULT_FLEET_VIEW;
	const expanded: Record<string, boolean> = {};
	if (saved.expanded && typeof saved.expanded === "object")
		for (const [project, open] of Object.entries(saved.expanded)) {
			if (typeof open !== "boolean") continue;
			expanded[project === here ? HERE_GROUP : HERE_GROUP + project] = open;
		}
	const search = typeof saved.search === "string" ? saved.search : "";
	return {
		filter: FLEET_FILTERS.find((filter) => filter === saved.filter) ?? "all",
		provider: typeof saved.provider === "string" ? saved.provider : "",
		search,
		searching: saved.searching === true || !!search,
		expanded,
	};
}
export interface FleetGroup {
	id: string;
	label: string;
	sessions: FleetSession[];
	/** The project of a group, when the list holds the sessions of several. */
	project?: { uri: string; here: boolean };
}
export type FleetQuery = {
	filter: FleetFilter;
	search?: string;
	provider?: string;
	/** The provider names a search also matches. */
	providerName?: (provider: string) => string;
	/** Whether a session belongs to the window's project. */
	here?: (session: FleetSession) => boolean;
	now?: Date;
};

const time = (value: string) => {
	const ms = new Date(value).getTime();
	return Number.isNaN(ms) ? 0 : ms;
};
export const newestFirst = (a: FleetSession, b: FleetSession) =>
	time(b.modifiedAt) - time(a.modifiedAt);

function matches(session: FleetSession, query: FleetQuery): boolean {
	if (query.provider && session.provider !== query.provider) return false;
	const search = query.search?.trim().toLowerCase();
	if (!search) return true;
	const names = [
		session.title,
		session.activity ?? "",
		session.project?.displayName ?? "",
		query.providerName?.(session.provider) ?? session.provider,
		...session.forks.map((fork) => fork.title),
	];
	return names.some((name) => name.toLowerCase().includes(search));
}

/** Sessions that need the user come first, then those that work, then the rest by day. */
export function groupFleet(
	sessions: readonly FleetSession[],
	query: FleetQuery,
): FleetGroup[] {
	const listed = sessions.filter((session) => matches(session, query));
	listed.sort(newestFirst);
	// Archived sessions that need input also stay visible in the main list.
	const waiting = listed.filter((s) => s.sessionState === "needs-input");
	const working = listed.filter(
		(s) => s.sessionState === "working" && !s.archived,
	);
	const rest = listed.filter(
		(s) =>
			s.sessionState !== "needs-input" &&
			s.sessionState !== "working" &&
			!s.archived,
	);
	const section = (id: string, label: string, items: FleetSession[]) =>
		items.length ? [{ id, label, sessions: items }] : [];
	// The sessions of several projects group by project; one project's by day.
	const here = query.here ?? (() => true);
	const projects = new Set(listed.map((s) => projectKey(s, here)));
	const settled = (items: FleetSession[]) =>
		projects.size > 1
			? byProject(items, here)
			: byDay(items, query.now ?? new Date());
	switch (query.filter) {
		case "needs-input":
			return section("needs-input", "Needs you", waiting);
		case "running":
			return section("working", "Working", working);
		case "archived":
			return settled(listed.filter((s) => s.archived));
		default:
			return [
				...section("needs-input", "Needs you", waiting),
				...section("working", "Working", working),
				...settled(rest),
			];
	}
}

/** The window's project is one group however the host spells its directory. */
function projectKey(
	session: FleetSession,
	here: (session: FleetSession) => boolean,
): string {
	return here(session) ? "" : (session.project?.uri ?? "");
}

function byProject(
	sessions: FleetSession[],
	here: (session: FleetSession) => boolean,
): FleetGroup[] {
	const groups = new Map<string, FleetGroup>();
	// Newest first, so projects follow their latest session.
	for (const session of sessions) {
		const key = projectKey(session, here);
		let group = groups.get(key);
		if (!group) {
			group = {
				id: `project:${key}`,
				label: session.project?.displayName ?? "Other",
				sessions: [],
				project: { uri: session.project?.uri ?? "", here: key === "" },
			};
			groups.set(key, group);
		}
		group.sessions.push(session);
	}
	// The window's project comes first; a stable sort keeps the others' order.
	return [...groups.values()].sort(
		(a, b) => Number(b.project?.here) - Number(a.project?.here),
	);
}

function byDay(sessions: FleetSession[], now: Date): FleetGroup[] {
	const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
	const yesterday = new Date(today);
	yesterday.setDate(today.getDate() - 1);
	const week = new Date(today);
	week.setDate(today.getDate() - 7);
	const groups = new Map<string, FleetSession[]>();
	for (const session of sessions) {
		const modified = new Date(session.modifiedAt);
		const label =
			Number.isNaN(modified.getTime()) || modified >= today
				? "Today"
				: modified >= yesterday
					? "Yesterday"
					: modified >= week
						? "This week"
						: "Older";
		const group = groups.get(label);
		if (group) group.push(session);
		else groups.set(label, [session]);
	}
	return [...groups].map(([label, items]) => ({
		id: label.toLowerCase().replace(" ", "-"),
		label,
		sessions: items,
	}));
}

export function fleetCounts(sessions: readonly FleetSession[]) {
	let needsInput = 0;
	let working = 0;
	for (const session of sessions) {
		if (session.sessionState === "needs-input") needsInput++;
		else if (session.sessionState === "working" && !session.archived) working++;
	}
	return { needsInput, working };
}

/** The chats that want the user, in the order a "next" command visits them:
 * those that wait for an answer, then those that finished or failed unseen. */
export function attentionQueue(sessions: readonly FleetSession[]): string[] {
	const ordered = [...sessions].sort(newestFirst);
	const queue: string[] = [];
	const chats = (session: FleetSession) => [session, ...session.forks];
	for (const session of ordered) {
		if (session.sessionState !== "needs-input") continue;
		const waiting = chats(session).filter(
			(chat) => chat.state === "needs-input",
		);
		if (waiting.length) queue.push(...waiting.map((chat) => chat.id));
		else queue.push(session.id);
	}
	for (const session of ordered) {
		const busy =
			session.sessionState === "needs-input" ||
			session.sessionState === "working";
		if (session.archived || busy) continue;
		for (const chat of chats(session)) if (chat.unread) queue.push(chat.id);
	}
	return queue;
}

/** The next chat to visit after the current one, wrapping around. */
export function nextAttention(
	sessions: readonly FleetSession[],
	current: string,
): string | undefined {
	const queue = attentionQueue(sessions);
	if (!queue.length) return;
	const index = queue.indexOf(current);
	const next = queue[(index + 1) % queue.length];
	return next === current ? undefined : next;
}

export type FleetEvent = {
	session: FleetSession;
	kind: "needs-input" | "finished" | "failed";
};

/** What became worth a look between two views of the sessions. */
export function fleetEvents(
	previous: ReadonlyMap<string, FleetState>,
	sessions: readonly FleetSession[],
): FleetEvent[] {
	const events: FleetEvent[] = [];
	for (const session of sessions) {
		const before = previous.get(session.session);
		const now = session.sessionState;
		if (!before || before === now) continue;
		if (now === "needs-input") events.push({ session, kind: "needs-input" });
		else if (before === "working" || before === "needs-input") {
			if (now === "failed") events.push({ session, kind: "failed" });
			else if (now === "idle") events.push({ session, kind: "finished" });
		}
	}
	return events;
}

export function relativeTime(value: string, now = new Date()): string {
	if (!value) return "";
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return "";
	const minutes = Math.floor((now.getTime() - date.getTime()) / 60000);
	if (minutes < 1) return "now";
	if (minutes < 60) return `${minutes}m`;
	const hours = Math.floor(minutes / 60);
	if (hours < 24) return `${hours}h`;
	const days = Math.floor(hours / 24);
	if (days < 7) return `${days}d`;
	return date.toLocaleDateString([], { month: "short", day: "numeric" });
}
