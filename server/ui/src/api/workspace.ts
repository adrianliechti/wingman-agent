import type { Bootstrap } from "../state/workspaceClient.ts";
import type { ShellEntry } from "../types/protocol.ts";
import { queryOptions } from "@tanstack/react-query";
import { fetchJSON } from "./http.ts";
import { queryKeys } from "./query.ts";

export interface WorkspaceInfo extends Bootstrap {
	name: string;
	path: string;
	shells: ShellEntry[];
	"window.terminal.position": "tab" | "bottom";
	"window.sidebar.position": "left" | "right";
	platform: string;
	/** A temporary chat's, which the host removes once no window shows it. */
	temporary?: boolean;
}

export const workspaceQuery = queryOptions({
	queryKey: queryKeys.workspace,
	queryFn: ({ signal }) =>
		fetchJSON<WorkspaceInfo>("/api/workspace", { signal }),
});

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
