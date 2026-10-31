import { fetchOK } from "./http.ts";
export interface WorkspaceSearchRequest {
	query: string;
	replacement: string;
	regex: boolean;
	case_sensitive: boolean;
	whole_word: boolean;
	include: string;
	exclude: string;
	limit?: number;
}

export interface WorkspaceSearchMatch {
	line: number;
	column: number;
	end_column: number;
	before: string;
	text: string;
	after: string;
	replacement: string;
}

export interface WorkspaceSearchFile {
	path: string;
	revision: string;
	matches: WorkspaceSearchMatch[];
}

export interface WorkspaceSearchSummary {
	files: number;
	matches: number;
	truncated: boolean;
}

export async function streamWorkspaceSearch(
	request: WorkspaceSearchRequest,
	onFile: (file: WorkspaceSearchFile) => void,
	signal?: AbortSignal,
): Promise<WorkspaceSearchSummary> {
	const response = await fetchOK("/api/workspace/search", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(request),
		signal,
	});
	if (!response.body) throw new Error("Search returned no results stream");
	const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
	let buffer = "";
	for (;;) {
		const { done, value } = await reader.read();
		buffer += value ?? "";
		const lines = buffer.split("\n");
		buffer = done ? "" : lines.pop()!;
		for (const line of lines) {
			if (!line.trim()) continue;
			const event = JSON.parse(line);
			if (event.type === "file") onFile(event.file);
			else if (event.type === "error") throw new Error(event.message);
			else if (event.type === "done")
				return {
					files: event.files,
					matches: event.matches,
					truncated: event.truncated,
				};
		}
		if (done) throw new Error("Search ended without a summary");
	}
}
