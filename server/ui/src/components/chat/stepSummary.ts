import type { ChatEntry } from "../../types/session";

export type ToolBucket =
	| "edit"
	| "delete"
	| "move"
	| "execute"
	| "read"
	| "search"
	| "fetch";

// Ordered by significance; file buckets count distinct locations, the rest count calls.
const phrases: Record<ToolBucket, [one: string, many: (n: number) => string]> =
	{
		edit: ["edited a file", (n) => `edited ${n} files`],
		delete: ["deleted a file", (n) => `deleted ${n} files`],
		move: ["moved a file", (n) => `moved ${n} files`],
		execute: ["ran a command", (n) => `ran ${n} commands`],
		read: ["read a file", (n) => `read ${n} files`],
		search: ["ran a search", (n) => `ran ${n} searches`],
		fetch: ["fetched a page", (n) => `fetched ${n} pages`],
	};
const order = Object.keys(phrases) as ToolBucket[];
const fileBuckets = new Set<ToolBucket>(["edit", "delete", "move", "read"]);
const maxPhrases = 3;

export function toolBucket(entry: ChatEntry): ToolBucket | null {
	if (entry.toolName === "Web search") return "search";
	const kind = entry.toolKind as ToolBucket;
	return kind in phrases ? kind : null;
}

// Summarizes a run of tool calls, e.g. "Edited 2 files, ran a command, read 3 files".
export function summarizeSteps(entries: ChatEntry[]): string {
	const buckets = new Map<ToolBucket, { calls: number; items: Set<string> }>();
	const others: string[] = [];
	for (const tool of entries) {
		if (tool.type !== "tool") continue;
		const bucket = toolBucket(tool);
		if (!bucket) {
			others.push(tool.toolName ?? "Tool");
			continue;
		}
		const group = buckets.get(bucket) ?? { calls: 0, items: new Set() };
		const paths = fileBuckets.has(bucket)
			? (tool.toolLocations ?? []).map((location) => location.path)
			: [];
		// A call without locations still counts once.
		for (const item of paths.length ? paths : [tool.id]) group.items.add(item);
		group.calls++;
		buckets.set(bucket, group);
	}

	const parts: string[] = [];
	let rest = others.length;
	for (const bucket of order) {
		const group = buckets.get(bucket);
		if (!group) continue;
		const n = group.items.size;
		if (parts.length === maxPhrases) rest += group.calls;
		else parts.push(n === 1 ? phrases[bucket][0] : phrases[bucket][1](n));
	}
	if (parts.length === 0) {
		const single = new Set(others).size === 1;
		parts.push(
			!single
				? `${others.length} steps`
				: others.length === 1
					? others[0]
					: `${others[0]} ×${others.length}`,
		);
	} else if (rest > 0) {
		parts.push(rest === 1 ? "1 other step" : `${rest} other steps`);
	}

	const label = parts.join(", ");
	return label.charAt(0).toUpperCase() + label.slice(1);
}
