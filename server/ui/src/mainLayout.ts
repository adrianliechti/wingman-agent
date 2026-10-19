import type { CompareMode, DiffLayer } from "./types/protocol";
import type { TabDisposition } from "./types/tabs";

export interface CenterTab {
	id: string;
	type:
		| "chat"
		| "file"
		| "diff"
		| "compare"
		| "terminal"
		| "debug"
		| "task"
		| "insights";
	label: string;
	path?: string;
	diffLayer?: DiffLayer;
	compareBase?: string;
	compareHead?: string;
	compareMode?: CompareMode;
	line?: number;
	column?: number;
	navigationKey?: number;
	external?: boolean;
	sessionId?: string;
	backendId?: string;
	terminalId?: string;
	taskId?: string;
	preview?: boolean;
	pane?: "right";
}

export type PaneSide = "left" | "right";

export function paneOf(tab: CenterTab): PaneSide {
	return tab.pane === "right" ? "right" : "left";
}

export const chatTabId = (sessionId: string) => `chat:${sessionId}`;

export function draftChatTab(
	backendId = "wingman",
	nonce: string = crypto.randomUUID(),
): CenterTab {
	return {
		id: `draft:${backendId}:${nonce}`,
		type: "chat",
		label: "Agent",
		backendId,
		sessionId: "",
	};
}

export function placeCenterTab(
	current: CenterTab[],
	candidate: CenterTab,
	disposition: TabDisposition,
	dirtyPaths: ReadonlySet<string>,
): { tabs: CenterTab[]; replaced?: CenterTab } {
	const existingIndex = current.findIndex((tab) => tab.id === candidate.id);
	if (existingIndex >= 0) {
		const existing = current[existingIndex];
		if (disposition !== "keep" || !existing.preview) return { tabs: current };
		const tabs = [...current];
		tabs[existingIndex] = { ...existing, preview: undefined };
		return { tabs };
	}

	const placed: CenterTab = {
		...candidate,
		preview: disposition === "preview" || undefined,
	};
	if (disposition === "keep") return { tabs: [...current, placed] };

	const previewIndex = current.findIndex(
		(tab) => tab.preview && paneOf(tab) === paneOf(placed),
	);
	if (previewIndex < 0) return { tabs: [...current, placed] };
	const previous = current[previewIndex];
	if (
		previous.type === "file" &&
		previous.path &&
		dirtyPaths.has(previous.path)
	) {
		const tabs = current.map((tab, index) =>
			index === previewIndex ? { ...tab, preview: undefined } : tab,
		);
		return { tabs: [...tabs, placed] };
	}
	const tabs = [...current];
	tabs[previewIndex] = placed;
	return { tabs, replaced: previous };
}

export function syncDebugTab(
	current: CenterTab[],
	terminalId: string | undefined,
	ensure: boolean,
	pane?: "right",
): CenterTab[] {
	let changed = false;
	let next = current;
	if (terminalId) {
		const filtered = current.filter(
			(tab) => tab.type !== "terminal" || tab.terminalId !== terminalId,
		);
		if (filtered.length !== current.length) {
			next = filtered;
			changed = true;
		}
	}

	const index = next.findIndex((tab) => tab.id === "debug");
	if (index < 0) {
		return ensure
			? [
					...next,
					{ id: "debug", type: "debug", label: "Debug", terminalId, pane },
				]
			: next;
	}
	if (next[index].terminalId === terminalId) return changed ? next : current;

	const updated = [...next];
	updated[index] = { ...updated[index], terminalId };
	return updated;
}

// Reorders a tab to the given strip position; the target index is expressed
// against the pre-removal list, as drop indicators report it.
export function moveTab(
	current: CenterTab[],
	tabId: string,
	index: number,
): CenterTab[] {
	const from = current.findIndex((tab) => tab.id === tabId);
	if (from < 0) return current;
	let to = Math.max(0, Math.min(index, current.length));
	if (from < to) to -= 1;
	if (to === from) return current;
	const next = [...current];
	const [tab] = next.splice(from, 1);
	next.splice(to, 0, tab);
	return next;
}

export type LayoutState = {
	tabs: CenterTab[];
	activeTabId: string;
	leftActiveId: string;
	rightActiveId: string;
	currentSessionId: string;
};
export type LayoutAction =
	| {
			type: "tabs";
			value: CenterTab[] | ((previous: CenterTab[]) => CenterTab[]);
	  }
	| { type: "activate"; id: string }
	| { type: "mapTabs"; transform: (tab: CenterTab) => CenterTab };

export function layoutReducer(
	state: LayoutState,
	action: LayoutAction,
): LayoutState {
	const next = { ...state };
	switch (action.type) {
		case "tabs":
			next.tabs =
				typeof action.value === "function"
					? action.value(state.tabs)
					: action.value;
			if (next.tabs === state.tabs) return state;
			break;
		case "activate":
			if (action.id === state.activeTabId) return state;
			next.activeTabId = action.id;
			break;
		case "mapTabs": {
			next.tabs = state.tabs.map(action.transform);
			// Tab IDs can change when files move or an untitled file is saved.
			// Remap every selection before repairing the layout.
			const ids = new Map(
				state.tabs.map((tab, i) => [tab.id, next.tabs[i].id]),
			);
			next.activeTabId = ids.get(state.activeTabId) ?? state.activeTabId;
			next.leftActiveId = ids.get(state.leftActiveId) ?? state.leftActiveId;
			next.rightActiveId = ids.get(state.rightActiveId) ?? state.rightActiveId;
			break;
		}
	}
	if (!next.tabs.some((tab) => paneOf(tab) === "left"))
		next.tabs = next.tabs.map((tab) => ({ ...tab, pane: undefined }));
	const active =
		next.tabs.find((tab) => tab.id === next.activeTabId) ?? next.tabs[0];
	next.activeTabId = active?.id ?? "";
	const left = next.tabs.filter((tab) => paneOf(tab) === "left");
	const right = next.tabs.filter((tab) => paneOf(tab) === "right");
	next.leftActiveId =
		active && paneOf(active) === "left"
			? active.id
			: (left.find((tab) => tab.id === next.leftActiveId)?.id ??
				left[0]?.id ??
				"");
	next.rightActiveId =
		active && paneOf(active) === "right"
			? active.id
			: (right.find((tab) => tab.id === next.rightActiveId)?.id ??
				right[0]?.id ??
				"");
	if (active?.type === "chat") next.currentSessionId = active.sessionId ?? "";
	else if (
		!next.tabs.some(
			(tab) => tab.type === "chat" && tab.sessionId === next.currentSessionId,
		)
	)
		next.currentSessionId =
			next.tabs.find((tab) => tab.type === "chat")?.sessionId ?? "";
	return next;
}
