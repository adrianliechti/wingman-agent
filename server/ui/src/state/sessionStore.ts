import type {
	ChatEntry,
	PendingPrompt,
	PendingTurnInput,
} from "../types/session.ts";
import type { Phase } from "../types/protocol.ts";
import type { ModelInfo } from "../api/models.ts";
import type { ModeOption } from "../api/sessions.ts";
import { summarizeSession, type SessionSummary } from "./sessionSummary.ts";

export type WorkspaceScope = { workspaceId: string; instanceId: string };
export type SessionSettings = {
	models: ModelInfo[];
	model: string;
	effort: string;
	efforts: string[];
	modes: ModeOption[];
	mode: string;
	unattended?: boolean;
	canFork?: boolean;
};
export type SettingsPatch = Partial<
	Pick<SessionSettings, "model" | "effort" | "mode" | "unattended">
>;
export type SessionFields = {
	status: "loading" | "ready" | "error" | "deleted";
	phase: Phase;
	/** The host's status line: a retry, tool progress or compaction. */
	activity?: string;
	error: string | null;
	usage: {
		inputTokens: number;
		cachedTokens: number;
		outputTokens: number;
		lastInputTokens: number;
		contextWindow: number;
	};
	prompts: PendingPrompt[];
	pendingInputs: PendingTurnInput[];
	settings: SessionSettings;
};
export type SessionView = SessionFields & {
	/** The chat URI; a tab shows one chat. */
	id: string;
	/** The URI of the session the chat belongs to. */
	session: string;
	provider: string;
	entries: ChatEntry[];
	revision: number;
	synchronized: boolean;
};

/** A chat that does not exist yet; real chats are keyed by their chat URI. */
export function draftKey(backendId: string): string {
	return JSON.stringify([backendId, ""]);
}
export function isDraft(key: string): boolean {
	return !key.startsWith("ahp-chat:");
}
export function draftBackend(key: string): string {
	try {
		const [backendId] = JSON.parse(key) as [unknown];
		if (typeof backendId === "string" && backendId) return backendId;
	} catch {
		// Not a draft key.
	}
	return "wingman";
}

function base64url(value: string): string {
	let binary = "";
	for (const byte of new TextEncoder().encode(value))
		binary += String.fromCharCode(byte);
	return btoa(binary)
		.replace(/\+/g, "-")
		.replace(/\//g, "_")
		.replace(/=+$/, "");
}
function fromBase64url(value: string): string {
	const binary = atob(value.replace(/-/g, "+").replace(/_/g, "/"));
	return new TextDecoder().decode(
		Uint8Array.from(binary, (char) => char.charCodeAt(0)),
	);
}
/** The host's canonical chat URI for a native session it has not aliased. */
export function canonicalChatURI(backendId: string, sessionId: string): string {
	return `ahp-chat:/wingman/${base64url(backendId)}/${base64url(sessionId)}`;
}
/** The provider a canonical chat or session URI names, if any. */
export function canonicalBackend(uri: string): string | undefined {
	const match = /^ahp-(?:chat|session):\/wingman\/([^/]+)\/[^/]+$/.exec(uri);
	if (!match) return;
	try {
		return fromBase64url(match[1]);
	} catch {
		return;
	}
}
export function chatToSession(uri: string): string {
	return uri.replace(/^ahp-chat:/, "ahp-session:");
}
export function sessionToChat(uri: string): string {
	return uri.replace(/^ahp-session:/, "ahp-chat:");
}
/** Keys were `["backend","sessionId"]` before URI identity; drafts stay drafts. */
export function migrateSessionKey(key: string): string {
	if (!key || key.startsWith("ahp-chat:")) return key;
	if (key.startsWith("ahp-session:")) return sessionToChat(key);
	try {
		const [backendId, sessionId] = JSON.parse(key) as [unknown, unknown];
		if (
			typeof backendId === "string" &&
			typeof sessionId === "string" &&
			sessionId
		)
			return canonicalChatURI(backendId, sessionId);
	} catch {
		// Not a legacy key.
	}
	return key;
}

export const EMPTY_SETTINGS: SessionSettings = {
	models: [],
	model: "",
	effort: "",
	efforts: [],
	modes: [],
	mode: "",
};
export function emptySession(id: string): SessionView {
	return {
		id,
		session: "",
		provider: "",
		revision: 0,
		synchronized: false,
		status: "loading",
		phase: "idle",
		entries: [],
		error: null,
		usage: {
			inputTokens: 0,
			cachedTokens: 0,
			outputTokens: 0,
			lastInputTokens: 0,
			contextWindow: 0,
		},
		prompts: [],
		pendingInputs: [],
		settings: EMPTY_SETTINGS,
	};
}
// React presentation cache. AHPConnection owns wire ordering and reducers;
// AHPSessionProjection maps channel state into these UI views.
export class SessionStore {
	// Presentation state comes from the official AHP reducers. The store only
	// batches React notifications and retains summaries for closed tabs.
	set(key: string, view: SessionView) {
		this.views = { ...this.views, [key]: view };
		this.emit(key);
	}
	private views: Record<string, SessionView> = {};
	private summaries: Record<string, SessionSummary> = {};
	readonly getSummaries = () => this.summaries;
	private renderListeners = new Set<() => void>();
	private frame: ReturnType<typeof setTimeout> | undefined;
	readonly subscribeRender = (listener: () => void) => {
		this.renderListeners.add(listener);
		return () => {
			this.renderListeners.delete(listener);
		};
	};
	private subscriptions = new Set<string>();
	private listeners = new Set<() => void>();
	readonly getSnapshot = () => this.views;
	readonly subscribe = (listener: () => void) => {
		this.listeners.add(listener);
		return () => {
			this.listeners.delete(listener);
		};
	};
	private emit(key?: string) {
		for (const id of key ? [key] : Object.keys(this.views)) {
			const summary = summarizeSession(this.views[id]);
			if (JSON.stringify(summary) !== JSON.stringify(this.summaries[id]))
				this.summaries = { ...this.summaries, [id]: summary };
		}
		for (const listener of this.listeners) listener();
		if (!this.frame && this.renderListeners.size)
			this.frame = setTimeout(() => {
				this.frame = undefined;
				for (const listener of this.renderListeners) listener();
			}, 16);
	}
	expect(key: string) {
		this.subscriptions.add(key);
		const previous = this.views[key] ?? emptySession(key);
		this.views = {
			...this.views,
			[key]: {
				...previous,
				synchronized: false,
				...(previous.status === "error"
					? ({ status: "loading", error: null } as const)
					: {}),
			},
		};
		this.emit(key);
	}
	forget(key: string) {
		this.subscriptions.delete(key);
		// Closed tabs can reload a snapshot. Bound cached histories separately
		// from draft ownership, while retaining active background sessions.
		const inactive = Object.keys(this.views).filter(
			(id) => !this.subscriptions.has(id) && this.views[id].phase === "idle",
		);
		if (inactive.length > 20) {
			this.views = { ...this.views };
			this.summaries = { ...this.summaries };
			for (const id of inactive.slice(0, inactive.length - 20)) {
				delete this.views[id];
				delete this.summaries[id];
			}
			this.emit();
		}
	}
	disconnect() {
		this.views = Object.fromEntries(
			Object.entries(this.views).map(([key, view]) => [
				key,
				{ ...view, synchronized: false },
			]),
		);
		this.emit();
	}
}
