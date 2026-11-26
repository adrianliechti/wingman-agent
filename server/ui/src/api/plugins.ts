import { queryOptions } from "@tanstack/react-query";
import { fetchJSON } from "./http.ts";
import { queryKeys } from "./query.ts";

export interface Plugin {
	name: string;
	version?: string;
	description?: string;
	scope: "personal" | "project";
	skills: number;
	mcp_servers: number;
	language_servers: number;
	hooks: boolean;
}

export const pluginsQuery = queryOptions({
	queryKey: queryKeys.plugins,
	queryFn: ({ signal }) =>
		fetchJSON<Plugin[]>("/api/workspace/plugins", { signal }),
});
