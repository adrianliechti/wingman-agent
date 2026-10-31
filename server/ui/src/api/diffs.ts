import type { DiffEntry, DiffLayer } from "../types/protocol";
import { workspaceClient } from "../state/workspaceClient.ts";
import { workspaceChangeset } from "../state/changesetClient.ts";

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
	return workspaceClient().changesets.files(
		workspaceChangeset(layer),
		path,
		true,
		signal,
	);
}
export async function revertDiff(path: string): Promise<void> {
	await workspaceClient().changesets.operate(
		workspaceChangeset("unstaged"),
		"discard-changes",
		[path],
	);
}
