import type {
	SessionInputRequest,
	SessionState,
} from "@microsoft/agent-host-protocol";
import {
	useEffect,
	useMemo,
	useRef,
	useState,
	useSyncExternalStore,
} from "react";
import { workspaceClient } from "../state/workspaceClient.ts";
import { isDraft } from "../state/sessionStore.ts";
import {
	fleetCounts,
	fleetEvents,
	fleetSession,
	type FleetEvent,
	type FleetFilter,
	type FleetSession,
	type FleetState,
} from "../state/fleet.ts";

export type FleetView = {
	filter: FleetFilter;
	provider: string;
	search: string;
	searching: boolean;
	/** Project groups the user opened or closed; others' groups start closed. */
	expanded: Readonly<Record<string, boolean>>;
};
// The list moves to each chat it opens; what it shows travels with it.
let fleetView: FleetView = {
	filter: "all",
	provider: "",
	search: "",
	searching: false,
	expanded: {},
};
const fleetViewListeners = new Set<() => void>();
function subscribeFleetView(listener: () => void) {
	fleetViewListeners.add(listener);
	return () => {
		fleetViewListeners.delete(listener);
	};
}
function setFleetView(patch: Partial<FleetView>) {
	fleetView = { ...fleetView, ...patch };
	for (const listener of fleetViewListeners) listener();
}
/** The session list's filter and search, shared by every place that shows the list. */
export function useFleetView() {
	const view = useSyncExternalStore(subscribeFleetView, () => fleetView);
	return [view, setFleetView] as const;
}

/** The host's sessions as the session list shows them. */
export function useFleet() {
	const catalog = workspaceClient().catalog;
	const state = useSyncExternalStore(catalog.subscribe, catalog.getSnapshot);
	const sessions = useMemo(
		() => state.sessions.map(fleetSession),
		[state.sessions],
	);
	return {
		sessions,
		loaded: state.loaded,
		error: state.error,
		starting: state.starting,
		counts: useMemo(() => fleetCounts(sessions), [sessions]),
	};
}

/** Whether a session belongs to the window's project, whichever way the host spells it. */
export function useHere(): (session: FleetSession) => boolean {
	const { directory, project } = workspaceClient().scope;
	return useMemo(() => {
		const uris = new Set([directory, project].filter(Boolean));
		// A host that names no directory shows one project.
		return (session: FleetSession) =>
			!uris.size || !session.project || uris.has(session.project.uri);
	}, [directory, project]);
}

const NO_REQUESTS: readonly SessionInputRequest[] = [];

/** The requests a session waits on, while it waits on any. */
export function useSessionInputNeeded(session: string, waiting: boolean) {
	const client = workspaceClient();
	const [requests, setRequests] = useState({
		session: "",
		items: NO_REQUESTS,
	});
	useEffect(() => {
		if (!waiting) return;
		return client.ahp.watch(
			session,
			(update) =>
				setRequests({
					session,
					items: (update.state as SessionState).inputNeeded ?? NO_REQUESTS,
				}),
			() => setRequests({ session, items: NO_REQUESTS }),
		);
	}, [client, session, waiting]);
	return waiting && requests.session === session ? requests.items : NO_REQUESTS;
}

export function useNow(interval = 60_000) {
	const [now, setNow] = useState(() => new Date());
	useEffect(() => {
		const timer = setInterval(() => setNow(new Date()), interval);
		return () => clearInterval(timer);
	}, [interval]);
	return now;
}

function subscribeVisibility(listener: () => void) {
	document.addEventListener("visibilitychange", listener);
	window.addEventListener("focus", listener);
	window.addEventListener("blur", listener);
	return () => {
		document.removeEventListener("visibilitychange", listener);
		window.removeEventListener("focus", listener);
		window.removeEventListener("blur", listener);
	};
}
const visible = () => document.visibilityState === "visible";

/** Chats the user marked unread; they stay unread while they remain on screen. */
const keptUnread = new Set<string>();
export function keepUnread(chat: string) {
	keptUnread.add(chat);
}

/** Marks the chats on screen read, again whenever the host reports new activity in them. */
export function useMarkShownRead(
	shown: readonly string[],
	sessions: readonly FleetSession[],
) {
	const client = workspaceClient();
	const onScreen = useSyncExternalStore(subscribeVisibility, visible);
	const unread = shown.filter(
		(key) =>
			!isDraft(key) &&
			sessions.some(
				(session) =>
					(session.id === key && session.unread) ||
					session.forks.some((fork) => fork.id === key && fork.unread),
			),
	);
	const visibleKeys = shown.join("\n");
	const pending = unread.join("\n");
	const requested = useRef(new Set<string>());
	const previousShown = useRef<string[]>([]);
	useEffect(() => {
		// Leaving a chat ends the user's wish to keep it unread.
		const showing = visibleKeys.split("\n");
		for (const key of keptUnread)
			if (!showing.includes(key) || !previousShown.current.includes(key))
				keptUnread.delete(key);
		previousShown.current = showing;
	}, [visibleKeys]);
	useEffect(() => {
		if (!onScreen) return;
		const keys = pending ? pending.split("\n") : [];
		// A key read meanwhile can be requested again once it is new again.
		for (const key of requested.current)
			if (!keys.includes(key)) requested.current.delete(key);
		for (const key of keys) {
			if (requested.current.has(key) || keptUnread.has(key)) continue;
			requested.current.add(key);
			void client.markRead(key, true).catch(() => {
				requested.current.delete(key);
			});
		}
	}, [client, onScreen, pending]);
}

/** The page title names the window's project, since tabs may show several,
 * and counts the sessions that wait for the user. */
export function useTitleCount(count: number, project?: string) {
	const base = useRef(document.title);
	useEffect(() => {
		const title = project ? `${project} – ${base.current}` : base.current;
		document.title = count > 0 ? `(${count}) ${title}` : title;
	}, [count, project]);
	useEffect(() => {
		const title = base.current;
		return () => {
			document.title = title;
		};
	}, []);
}

const NOTIFICATIONS = "wingman.notifications";
/** System notifications work in browsers; the desktop app's web view exposes
 * the API but never grants it, and has no settings to allow it in. */
export function notificationsSupported() {
	return (
		typeof window !== "undefined" && "Notification" in window && !window.shell
	);
}
function notificationsWanted() {
	try {
		return localStorage.getItem(NOTIFICATIONS) === "on";
	} catch {
		return false;
	}
}
function subscribeNotifications(listener: () => void) {
	window.addEventListener("storage", listener);
	window.addEventListener("wingman:notifications", listener);
	return () => {
		window.removeEventListener("storage", listener);
		window.removeEventListener("wingman:notifications", listener);
	};
}
const notificationsOn = () =>
	notificationsSupported() &&
	Notification.permission === "granted" &&
	notificationsWanted();

/** Whether system notifications are on, and a switch that asks for permission. */
export function useNotificationSetting() {
	const enabled = useSyncExternalStore(subscribeNotifications, notificationsOn);
	const set = async (on: boolean) => {
		if (on && Notification.permission !== "granted") {
			const permission = await Notification.requestPermission();
			if (permission !== "granted") return false;
		}
		try {
			if (on) localStorage.setItem(NOTIFICATIONS, "on");
			else localStorage.removeItem(NOTIFICATIONS);
		} catch {
			return false;
		}
		window.dispatchEvent(new Event("wingman:notifications"));
		return true;
	};
	return { supported: notificationsSupported(), enabled, set };
}

const EVENT_TITLES: Record<FleetEvent["kind"], string> = {
	"needs-input": "Needs your input",
	finished: "Finished",
	failed: "Failed",
};

/** Tells the user through the system, while the page is not in front, when a
 * session waits for them, finishes or fails. */
export function useFleetNotifications(
	sessions: readonly FleetSession[],
	loaded: boolean,
	open: (id: string) => void,
) {
	const previous = useRef<Map<string, FleetState> | null>(null);
	const openRef = useRef(open);
	useEffect(() => {
		openRef.current = open;
	});
	useEffect(() => {
		if (!loaded) return;
		const before = previous.current;
		previous.current = new Map(
			sessions.map((session) => [session.session, session.sessionState]),
		);
		if (!before || !notificationsOn()) return;
		if (document.visibilityState === "visible" && document.hasFocus()) return;
		for (const event of fleetEvents(before, sessions)) {
			const { session } = event;
			const body =
				event.kind === "needs-input" && session.activity
					? `${session.title}\n${session.activity}`
					: session.title;
			try {
				const notification = new Notification(EVENT_TITLES[event.kind], {
					body,
					tag: session.session,
				});
				notification.onclick = () => {
					window.focus();
					const chats = [session, ...session.forks];
					const target = chats.find((chat) =>
						event.kind === "needs-input"
							? chat.state === "needs-input"
							: event.kind === "failed"
								? chat.state === "failed"
								: chat.unread,
					);
					openRef.current(target?.id ?? session.id);
					notification.close();
				};
			} catch {
				// Some embedded browsers expose the API but refuse to show notifications.
			}
		}
	}, [sessions, loaded]);
}
