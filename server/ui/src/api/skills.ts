import { queryOptions } from "@tanstack/react-query";
import { fetchJSON } from "./http.ts";
import { queryKeys } from "./query.ts";

export interface Skill {
	name: string;
	description?: string;
	input_hint?: string;
	scope?: "builtin" | "plugin" | "personal" | "project";
}

export function listWorkspaceSkills(signal?: AbortSignal) {
	return fetchJSON<Skill[]>("/api/workspace/skills", { signal });
}

export const skillCatalogueQuery = queryOptions({
	queryKey: queryKeys.skills.catalogue,
	queryFn: ({ signal }) => listWorkspaceSkills(signal),
});
