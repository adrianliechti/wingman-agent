import type { SessionSummary } from "@microsoft/agent-host-protocol";
import { SessionStatus } from "./ahpValues.ts";
import { sessionToChat } from "./sessionStore.ts";

// InputNeeded also sets InProgress: the bit of its own tells the two apart.
const INPUT_NEEDED = SessionStatus.InputNeeded & ~SessionStatus.InProgress;

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
	/** `modifiedAt` as the list sorts by it: milliseconds, 0 for no date. */
	modified: number;
	project?: { uri: string; displayName: string };
	forks: FleetChat[];
}

export function fleetState(status: number = SessionStatus.Idle): FleetState {
	if (status & INPUT_NEEDED) return "needs-input";
	if (status & SessionStatus.InProgress) return "working";
	if (status & SessionStatus.Error) return "failed";
	return "idle";
}

/** Derives a value from an object once. The host's summaries and the lists of
 * them are replaced when they change, never changed: every place that shows
 * the sessions shares what derives from them, and an unchanged session keeps
 * its object. */
function once<K extends object, V>(derive: (key: K) => V): (key: K) => V {
	const derived = new WeakMap<K, V>();
	return (key) => {
		let value = derived.get(key);
		if (value === undefined) {
			value = derive(key);
			derived.set(key, value);
		}
		return value;
	};
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

export const fleetSession = once((summary: SessionSummary): FleetSession => {
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
			unread:
				chat.status !== undefined && (chat.status & SessionStatus.IsRead) === 0,
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
		unread: (status & SessionStatus.IsRead) === 0,
		archived: (summary.status & SessionStatus.IsArchived) !== 0,
		activity: summary.activity || undefined,
		modifiedAt: summary.modifiedAt,
		modified: new Date(summary.modifiedAt).getTime() || 0,
		project: summary.project,
		changes: changesOf(summary.changes ?? own?.changes),
		forks,
	};
});

/** The host's list as sessions, in the host's order. */
export const fleetSessions = once(
	(summaries: readonly SessionSummary[]): readonly FleetSession[] =>
		summaries.map(fleetSession),
);

/** A list's sessions, newest first. */
export const newestSessions = once(
	(sessions: readonly FleetSession[]): readonly FleetSession[] =>
		[...sessions].sort((a, b) => b.modified - a.modified),
);

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
/** A project's group is named by the project's URI, in every window alike. */
const projectGroup = (uri: string) => `project:${uri}`;

/** A list's view as the window's next page finds it again. The window's own
 * project is recorded open, as its group is until the user closes it: the
 * list of a project the window moves to would start it closed. */
export function storeFleetView(view: FleetView, project?: string): string {
	if (!project) return JSON.stringify(view);
	const own = projectGroup(project);
	return JSON.stringify({
		...view,
		expanded: { ...view.expanded, [own]: view.expanded[own] ?? true },
	});
}

/** Reads a stored view; one that is missing or damaged starts afresh. */
export function restoreFleetView(stored: string | null): FleetView {
	let saved: Partial<Record<keyof FleetView, unknown>>;
	try {
		saved = JSON.parse(stored ?? "") ?? {};
	} catch {
		return DEFAULT_FLEET_VIEW;
	}
	if (typeof saved !== "object") return DEFAULT_FLEET_VIEW;
	const expanded: Record<string, boolean> = {};
	if (saved.expanded && typeof saved.expanded === "object")
		for (const [group, open] of Object.entries(saved.expanded))
			if (typeof open === "boolean") expanded[group] = open;
	const search = typeof saved.search === "string" ? saved.search : "";
	return {
		filter: FLEET_FILTERS.find((filter) => filter === saved.filter) ?? "all",
		provider: typeof saved.provider === "string" ? saved.provider : "",
		search,
		searching: saved.searching === true || !!search,
		expanded,
	};
}

/** Where a list's view waits for the window's next page. */
export interface FleetViewStorage {
	read(): string | null;
	write(view: string): void;
}

/** The session list's view, shared by every place that shows the list. The
 * list moves to each chat it opens, and the window to each project: the view
 * travels with it, through the reload that shows the project. */
export class FleetViewStore {
	private view?: FleetView;
	private listeners = new Set<() => void>();
	private readonly storage: FleetViewStorage;
	/** The URI of the window's project. */
	private readonly project?: string;
	constructor(storage: FleetViewStorage, project?: string) {
		this.storage = storage;
		this.project = project;
	}
	// Read at first use: a client is also built where there is no browser.
	readonly getSnapshot = (): FleetView =>
		(this.view ??= restoreFleetView(this.storage.read()));
	readonly subscribe = (listener: () => void) => {
		this.listeners.add(listener);
		return () => {
			this.listeners.delete(listener);
		};
	};
	readonly set = (patch: Partial<FleetView>) => {
		this.view = { ...this.getSnapshot(), ...patch };
		this.keep();
		for (const listener of this.listeners) listener();
	};
	/** Keeps the view for the window's next page, such as the project it moves
	 * to, where the project it leaves is listed open as it was shown. */
	keep() {
		this.storage.write(storeFleetView(this.getSnapshot(), this.project));
	}
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
	/** The URI of the window's project, which names the group of its sessions. */
	ownProject?: string;
	now?: Date;
};

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
	const here = query.here ?? (() => true);
	const own = query.ownProject ?? "";
	const waiting: FleetSession[] = [];
	const working: FleetSession[] = [];
	const rest: FleetSession[] = [];
	const archived: FleetSession[] = [];
	const projects = new Set<string>();
	// One pass over the list newest first, so each part of it is too.
	for (const session of newestSessions(sessions)) {
		if (!matches(session, query)) continue;
		projects.add(projectKey(session, here, own));
		if (session.archived) archived.push(session);
		// Archived sessions that need input also stay visible in the main list.
		if (session.sessionState === "needs-input") waiting.push(session);
		else if (!session.archived) {
			if (session.sessionState === "working") working.push(session);
			else rest.push(session);
		}
	}
	const section = (id: string, label: string, items: FleetSession[]) =>
		items.length ? [{ id, label, sessions: items }] : [];
	// The sessions of several projects group by project; one project's by day.
	const settled = (items: FleetSession[]) =>
		projects.size > 1
			? byProject(items, here, own)
			: byDay(items, query.now ?? new Date());
	switch (query.filter) {
		case "needs-input":
			return section("needs-input", "Needs you", waiting);
		case "running":
			return section("working", "Working", working);
		case "archived":
			return settled(archived);
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
	own: string,
): string {
	return here(session) ? own : (session.project?.uri ?? "");
}

function byProject(
	sessions: FleetSession[],
	here: (session: FleetSession) => boolean,
	own: string,
): FleetGroup[] {
	const groups = new Map<string, FleetGroup>();
	// Newest first, so projects follow their latest session.
	for (const session of sessions) {
		const key = projectKey(session, here, own);
		let group = groups.get(key);
		if (!group) {
			group = {
				id: projectGroup(key),
				label: session.project?.displayName ?? "Other",
				sessions: [],
				project: { uri: session.project?.uri ?? "", here: key === own },
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

export const fleetCounts = once((sessions: readonly FleetSession[]) => {
	let needsInput = 0;
	let working = 0;
	for (const session of sessions) {
		if (session.sessionState === "needs-input") needsInput++;
		else if (session.sessionState === "working" && !session.archived) working++;
	}
	return { needsInput, working };
});

/** The chats that want the user, in the order a "next" command visits them:
 * those that wait for an answer, then those that finished or failed unseen. */
export function attentionQueue(sessions: readonly FleetSession[]): string[] {
	const ordered = newestSessions(sessions);
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
