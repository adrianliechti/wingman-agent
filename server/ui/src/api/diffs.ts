import type { DiffEntry, DiffLayer } from "../types/protocol";
import { workspaceClient } from "../state/workspaceClient.ts";

interface ListDiffsOptions {
	layer?: DiffLayer;
	path?: string;
	signal?: AbortSignal;
}
export function listDiffs({
	layer,
	path,
	signal,
}: ListDiffsOptions = {}): Promise<DiffEntry[]> {
	const changesets = workspaceClient().changesets;
	return changesets.files(changesets.layer(layer), path, true, signal);
}
export async function revertDiff(path: string): Promise<void> {
	const changesets = workspaceClient().changesets;
	await changesets.operate(changesets.layer("unstaged"), "discard-changes", [
		path,
	]);
}
