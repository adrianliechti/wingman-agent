import type { Bootstrap } from "../state/workspaceClient.ts";
import type { ShellEntry } from "../types/protocol.ts";
import { queryOptions } from "@tanstack/react-query";
import { fetchJSON } from "./http.ts";
import { queryKeys } from "./query.ts";

export interface WorkspaceInfo extends Omit<Bootstrap, "backends"> {
	name: string;
	path: string;
	shells: ShellEntry[];
	lsp: boolean;
	debug: boolean;
	"window.terminal.position": "tab" | "bottom";
	"window.sidebar.position": "left" | "right";
	platform: string;
	managed_tools?: ManagedToolsStatus;
}

export interface ManagedToolsStatus {
	state: "installing" | "ready" | "error";
	tool?: string;
	label?: string;
	phase?: "checking" | "installing" | "updating";
	current?: number;
	total?: number;
	error?: string;
	unavailable?: string[];
}

export const workspaceQuery = queryOptions({
	queryKey: queryKeys.workspace,
	queryFn: ({ signal }) =>
		fetchJSON<WorkspaceInfo>("/api/workspace", { signal }),
});

export interface InspectAvailability {
	inspect: boolean;
	diagnostics: boolean;
	debug: boolean;
}

export function getInspectAvailability(
	capabilities: Pick<WorkspaceInfo, "lsp" | "debug"> | null,
): InspectAvailability {
	const diagnostics = capabilities?.lsp ?? false;
	const debug = capabilities?.debug ?? false;
	return { inspect: diagnostics, diagnostics, debug };
}

export interface GitWeekActivity {
	week: string;
	commits: number;
}

export interface GitAuthorStat {
	name: string;
	commits: number;
	files: number;
	last: string;
}

export interface GitChurnStat {
	file: string;
	commits: number;
	authors: number;
	recent_authors: { name: string; commits: number }[];
}

export interface GitAuthorSeries {
	name: string;
	weeks: number[];
}

export interface GitModuleActivity {
	module: string;
	commits: number;
}

export interface GitInsights {
	commits: number;
	total_authors: number;
	since: string;
	weeks: GitWeekActivity[];
	author_weeks: GitAuthorSeries[];
	punch: number[][];
	authors: GitAuthorStat[];
	modules: GitModuleActivity[];
	churn: GitChurnStat[];
}

export function getGitInsights(signal?: AbortSignal): Promise<GitInsights> {
	return fetchJSON("/api/workspace/insights", { signal });
}
