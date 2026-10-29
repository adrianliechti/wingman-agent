import { fetchJSON } from "./http.ts";

export interface Skill {
	name: string;
	description?: string;
	input_hint?: string;
}

export function listWorkspaceSkills(signal?: AbortSignal) {
	return fetchJSON<Skill[]>("/api/workspace/skills", { signal });
}
