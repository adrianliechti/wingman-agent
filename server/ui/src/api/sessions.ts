import { workspaceClient } from "../state/workspaceClient.ts";

export interface ModeOption {
	id: string;
	name: string;
	description?: string;
}
export async function deleteSession(key: string): Promise<void> {
	await workspaceClient().dispose(key);
}
export function forkSession(
	key: string,
	title?: string,
	turnId?: string,
): Promise<string> {
	return workspaceClient().fork(key, title, turnId);
}
