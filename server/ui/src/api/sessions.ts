import { queryOptions } from "@tanstack/react-query";
import { queryKeys } from "./query.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
export interface SessionInfo {
	id: string;
	title?: string;
	updated_at: string;
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
	await workspaceClient().command(key, { type: "delete" });
}
export function forkSession(key: string, title?: string): Promise<string> {
	return workspaceClient().fork(key, title);
}
export const sessionQueries = {
	list: (backend: string) =>
		queryOptions({
			queryKey: queryKeys.sessions.list(backend),
			queryFn: () => workspaceClient().list(backend),
		}),
};
