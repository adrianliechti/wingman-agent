import { queryOptions } from "@tanstack/react-query";
import { fetchJSON } from "./http.ts";
import { queryKeys } from "./query.ts";

export type McpServerState =
	| "pending"
	| "connecting"
	| "connected"
	| "failed"
	| "unapproved"
	| "login_required"
	| "disconnected";

export interface McpTool {
	name: string;
	description?: string;
}

export interface McpServer {
	name: string;
	transport?: string;
	target?: string;
	scope?: string;
	state: McpServerState;
	error?: string;
	tools: McpTool[];
}

export const mcpServersQuery = queryOptions({
	queryKey: queryKeys.mcp,
	queryFn: ({ signal }) =>
		fetchJSON<McpServer[]>("/api/workspace/mcp", { signal }),
});
