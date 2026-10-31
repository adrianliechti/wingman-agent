import { queryOptions } from "@tanstack/react-query";
import { queryKeys } from "./query.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
/** A session in the catalogue, opened through its default chat `id`. */
export interface SessionInfo {
	id: string;
	session: string;
	title?: string;
	updated_at: string;
	/** The default chat first, then forks. */
	chats: { id: string; title?: string }[];
}
export interface ModeOption {
	id: string;
	name: string;
	description?: string;
}
export interface ModeState {
	modes: ModeOption[];
	mode: string;
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
export const sessionQueries = {
	list: (backend: string) =>
		queryOptions({
			queryKey: queryKeys.sessions.list(backend),
			queryFn: () => workspaceClient().list(backend),
		}),
};
