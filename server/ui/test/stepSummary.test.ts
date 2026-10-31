import assert from "node:assert/strict";
import test from "node:test";
import type { ChatEntry } from "../src/types/session.ts";
import { summarizeSteps } from "../src/components/chat/stepSummary.ts";

let next = 0;
const tool = (
	toolName: string,
	toolKind: string,
	paths: string[] = [],
	toolHint?: string,
): ChatEntry => ({
	id: `tool-${next++}`,
	type: "tool",
	content: "",
	toolName,
	toolKind,
	toolHint,
	toolLocations: paths.map((path) => ({ path })),
});

test("describes tool runs by what they did", () => {
	assert.equal(
		summarizeSteps([
			tool("Read file", "read", ["server/main.go"]),
			tool("Read file", "read", ["src/main.tsx"]),
			tool("Run command", "execute"),
			tool("Edit file", "edit", ["server/main.go", "src/main.tsx"]),
		]),
		"Edited 2 files, ran a command, read 2 files",
	);
});

test("counts distinct files across repeated edits", () => {
	assert.equal(
		summarizeSteps([
			tool("Edit file", "edit", ["a.go", "b.go"]),
			tool("Edit file", "edit", ["a.go", "b.go"]),
			tool("Edit file", "edit", ["b.go"]),
		]),
		"Edited 2 files",
	);
});

test("ignores file counts in command and search hints", () => {
	assert.equal(
		summarizeSteps([
			tool("Run command", "execute", [], "wc -l 12 files.txt"),
			tool("Web search", "fetch", [], "find 50 files fast"),
		]),
		"Ran a command, ran a search",
	);
});

test("counts calls without locations once each", () => {
	assert.equal(
		summarizeSteps([tool("Read file", "read"), tool("Read file", "read")]),
		"Read 2 files",
	);
});

test("folds phrases past the third into other steps", () => {
	assert.equal(
		summarizeSteps([
			tool("Edit file", "edit", ["a.go"]),
			tool("Delete file", "delete", ["b.go"]),
			tool("Run command", "execute"),
			tool("Search files", "search"),
			tool("Search files", "search"),
			tool("Delegate task", ""),
		]),
		"Edited a file, deleted a file, ran a command, 3 other steps",
	);
});

test("names unclassified tools when nothing else ran", () => {
	assert.equal(
		summarizeSteps([tool("Delegate task", ""), tool("Delegate task", "")]),
		"Delegate task ×2",
	);
	assert.equal(
		summarizeSteps([tool("Delegate task", ""), tool("Message agent", "")]),
		"2 steps",
	);
});
