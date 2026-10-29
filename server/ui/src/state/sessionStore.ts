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
export type SessionRef = {
	workspaceId: string;
	backendId: string;
	sessionId: string;
};
export type SessionSettings = {
	models: ModelInfo[];
	modelsError?: string;
	model: string;
	effort: string;
	efforts: string[];
	modes: ModeOption[];
	mode: string;
	unattended?: boolean;
	canDelete: boolean;
	canFork?: boolean;
	canCompact?: boolean;
};
export type SettingsPatch = Partial<
	Pick<SessionSettings, "model" | "effort" | "mode" | "unattended">
>;
export type SessionFields = {
	status: "loading" | "ready" | "error" | "deleted";
	phase: Phase;
	retry?: { attempt: number; reason: string; delayMillis: number };
	error: string | null;
	compaction?: { tokensBefore: number; tokensAfter: number };
	usage: {
		inputTokens: number;
		cachedTokens: number;
		outputTokens: number;
		lastInputTokens: number;
		contextWindow: number;
	};
	prompts: PendingPrompt[];
	pendingInputs: PendingTurnInput[];
	queuePaused: boolean;
	canSteer: boolean;
	settings: SessionSettings;
	toolProgress: Record<string, string>;
};
export type SessionView = SessionFields & {
	id: string;
	entries: ChatEntry[];
	epoch: string;
	revision: number;
	synchronized: boolean;
};

export function sessionKey(backendId: string, sessionId: string): string {
	return JSON.stringify([backendId, sessionId]);
}
export function splitSessionKey(key: string): {
	backendId: string;
	sessionId: string;
} {
	if (!key) return { backendId: "wingman", sessionId: "" };
	const [backendId, sessionId] = JSON.parse(key) as [string, string];
	return { backendId, sessionId };
}
export function isDraft(key: string): boolean {
	return !splitSessionKey(key).sessionId;
}

export const EMPTY_SETTINGS: SessionSettings = {
	models: [],
	model: "",
	effort: "",
	efforts: [],
	modes: [],
	mode: "",
	canDelete: false,
};
export function emptySession(id: string): SessionView {
	return {
		id,
		epoch: "",
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
		queuePaused: false,
		canSteer: false,
		settings: EMPTY_SETTINGS,
		toolProgress: {},
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
