import assert from "node:assert/strict";
import test from "node:test";
import {
	type CenterTab,
	draftChatTab,
	layoutReducer,
	type LayoutState,
	moveTab,
	placeCenterTab,
	syncDebugTab,
} from "../src/mainLayout.ts";

const chat = (sessionId = "s1"): CenterTab => ({
	id: `chat:${sessionId}`,
	type: "chat",
	label: "Session",
	sessionId,
});

const file = (path: string): CenterTab => ({
	id: `file:${path}`,
	type: "file",
	label: path.split("/").pop() ?? path,
	path,
});

const terminal = (id: string): CenterTab => ({
	id: `terminal:${id}`,
	type: "terminal",
	label: "zsh",
	terminalId: id,
});

test("placeCenterTab replaces an existing preview tab", () => {
	const preview: CenterTab = { ...file("a.go"), preview: true };
	const { tabs, replaced } = placeCenterTab(
		[chat(), preview],
		file("b.go"),
		"preview",
		new Set(),
	);
	assert.equal(replaced?.id, "file:a.go");
	assert.deepEqual(
		tabs.map((t) => t.id),
		["chat:s1", "file:b.go"],
	);
	assert.equal(tabs[1].preview, true);
});

test("placeCenterTab keeps a dirty preview tab", () => {
	const preview: CenterTab = { ...file("a.go"), preview: true };
	const { tabs, replaced } = placeCenterTab(
		[chat(), preview],
		file("b.go"),
		"preview",
		new Set(["a.go"]),
	);
	assert.equal(replaced, undefined);
	assert.deepEqual(
		tabs.map((t) => t.id),
		["chat:s1", "file:a.go", "file:b.go"],
	);
	assert.equal(tabs[1].preview, undefined);
});

test("placeCenterTab promotes an existing preview on keep", () => {
	const preview: CenterTab = { ...file("a.go"), preview: true };
	const { tabs } = placeCenterTab(
		[chat(), preview],
		file("a.go"),
		"keep",
		new Set(),
	);
	assert.equal(tabs[1].preview, undefined);
});

test("preview replacement stays within the candidate's pane", () => {
	const leftPreview: CenterTab = { ...file("left.go"), preview: true };
	const rightPreview: CenterTab = {
		...file("right.go"),
		preview: true,
		pane: "right",
	};
	const { tabs, replaced } = placeCenterTab(
		[chat(), leftPreview, rightPreview],
		{ ...file("new.go"), pane: "right" },
		"preview",
		new Set(),
	);
	assert.equal(replaced?.id, "file:right.go");
	assert.deepEqual(
		tabs.map((t) => t.id),
		["chat:s1", "file:left.go", "file:new.go"],
	);
	assert.equal(tabs[2].pane, "right");
	assert.equal(tabs[1].preview, true);
});

test("syncDebugTab absorbs the matching terminal tab", () => {
	const tabs = syncDebugTab([chat(), terminal("t1")], "t1", true);
	assert.deepEqual(
		tabs.map((t) => t.id),
		["chat:s1", "debug"],
	);
	assert.equal(tabs[1].terminalId, "t1");
});

test("syncDebugTab updates the terminal id in place", () => {
	const existing: CenterTab[] = [
		chat(),
		{ id: "debug", type: "debug", label: "Debug", terminalId: "t1" },
	];
	const tabs = syncDebugTab(existing, undefined, false);
	assert.equal(tabs[1].terminalId, undefined);
});

test("moveTab adjusts for the removal shift when moving right", () => {
	const tabs = moveTab([chat(), file("a.go"), file("b.go")], "chat:s1", 3);
	assert.deepEqual(
		tabs.map((t) => t.id),
		["file:a.go", "file:b.go", "chat:s1"],
	);
});

test("moveTab moves left without adjustment", () => {
	const tabs = moveTab([chat(), file("a.go"), file("b.go")], "file:b.go", 0);
	assert.deepEqual(
		tabs.map((t) => t.id),
		["file:b.go", "chat:s1", "file:a.go"],
	);
});

test("moveTab is a no-op for same position or unknown ids", () => {
	const tabs = [chat(), file("a.go")];
	assert.equal(moveTab(tabs, "chat:s1", 0), tabs);
	assert.equal(moveTab(tabs, "missing", 1), tabs);
});

test("new drafts have distinct request identities", () => {
	const first = draftChatTab();
	const second = draftChatTab();
	assert.notEqual(first.id, second.id);
	assert.equal(first.backendId, "wingman");
});

test("layout reducer closes a selected session and repairs both pane selections atomically", () => {
	const first = chat("one");
	const second = { ...chat("two"), pane: "right" as const };
	const next = layoutReducer(
		{
			tabs: [first, second],
			activeTabId: second.id,
			leftActiveId: first.id,
			rightActiveId: second.id,
			currentSessionId: "two",
		},
		{ type: "tabs", value: [first] },
	);
	assert.equal(next.activeTabId, first.id);
	assert.equal(next.currentSessionId, "one");
	assert.equal(next.rightActiveId, "");
});
test("layout reducer promotes the remaining pane after the last left tab closes", () => {
	const first = chat("one");
	const second = { ...chat("two"), pane: "right" as const };
	const next = layoutReducer(
		{
			tabs: [first, second],
			activeTabId: second.id,
			leftActiveId: first.id,
			rightActiveId: second.id,
			currentSessionId: "two",
		},
		{ type: "tabs", value: [second] },
	);
	assert.equal(next.tabs[0].pane, undefined);
	assert.equal(next.leftActiveId, second.id);
	assert.equal(next.rightActiveId, "");
});

test("closing the final session leaves the center workspace empty", () => {
	const last = { ...chat("native"), backendId: "two" };
	const state = {
		tabs: [last],
		activeTabId: last.id,
		leftActiveId: last.id,
		rightActiveId: "",
		currentSessionId: "native",
	};
	const action = {
		type: "tabs",
		value: [],
	} as const;
	const first = layoutReducer(state, { ...action, value: [] });
	const replay = layoutReducer(state, { ...action, value: [] });
	assert.deepEqual(first, replay);
	assert.deepEqual(first.tabs, []);
	assert.equal(first.activeTabId, "");
	assert.equal(first.leftActiveId, "");
	assert.equal(first.rightActiveId, "");
	assert.equal(first.currentSessionId, "");
});

test("renaming files preserves both pane selections and the current session", () => {
	const first = chat("one");
	const left = file("src/left.go");
	const right = { ...file("src/right.go"), pane: "right" as const };
	const state: LayoutState = {
		tabs: [
			first,
			chat("two"),
			left,
			{ ...file("other.go"), pane: "right" },
			right,
		],
		activeTabId: left.id,
		leftActiveId: left.id,
		rightActiveId: right.id,
		currentSessionId: "two",
	};
	const next = layoutReducer(state, {
		type: "mapTabs",
		transform: (tab) => {
			if (!tab.path?.startsWith("src/")) return tab;
			const path = tab.path.replace("src/", "lib/");
			return { ...tab, id: `file:${path}`, path };
		},
	});
	assert.equal(next.activeTabId, "file:lib/left.go");
	assert.equal(next.leftActiveId, "file:lib/left.go");
	assert.equal(next.rightActiveId, "file:lib/right.go");
	assert.equal(next.currentSessionId, "two");
	assert.equal(state.tabs[2].path, "src/left.go");
});

test("saving an untitled file preserves its pane and background session", () => {
	const draft = { ...file("untitled:one"), pane: "right" as const };
	const state: LayoutState = {
		tabs: [chat("one"), chat("two"), draft],
		activeTabId: draft.id,
		leftActiveId: "chat:two",
		rightActiveId: draft.id,
		currentSessionId: "two",
	};
	const next = layoutReducer(state, {
		type: "mapTabs",
		transform: (tab) =>
			tab.id === draft.id ? { ...tab, ...file("saved.go") } : tab,
	});
	assert.equal(next.activeTabId, "file:saved.go");
	assert.equal(next.leftActiveId, "chat:two");
	assert.equal(next.rightActiveId, "file:saved.go");
	assert.equal(next.tabs[2].pane, "right");
	assert.equal(next.currentSessionId, "two");
});

test("tab activation and removal maintain the session without separate setters", () => {
	const first = chat("one");
	const second = { ...chat("two"), pane: "right" as const };
	const editor = file("main.go");
	let state: LayoutState = {
		tabs: [first, second, editor],
		activeTabId: first.id,
		leftActiveId: first.id,
		rightActiveId: second.id,
		currentSessionId: "one",
	};
	state = layoutReducer(state, { type: "activate", id: second.id });
	assert.equal(state.currentSessionId, "two");
	assert.equal(state.rightActiveId, second.id);
	state = layoutReducer(state, { type: "activate", id: editor.id });
	assert.equal(state.currentSessionId, "two");
	assert.equal(state.leftActiveId, editor.id);
	state = layoutReducer(state, {
		type: "tabs",
		value: (tabs) => tabs.filter((tab) => tab.id !== second.id),
	});
	assert.equal(state.currentSessionId, "one");
	assert.equal(state.activeTabId, editor.id);
	assert.equal(state.rightActiveId, "");
});
