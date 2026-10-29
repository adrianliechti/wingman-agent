import { workspaceClient } from "../state/workspaceClient.ts";
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
	signal?.throwIfAborted();
	const ahp = workspaceClient().ahp;
	await ahp.ready();
	signal?.throwIfAborted();
	const searchId = crypto.randomUUID();
	const release = ahp.onNotification((method, params) => {
		if (
			method === "x-wingman/searchProgress" &&
			params.searchId === searchId &&
			!signal?.aborted
		)
			onFile(params.file);
	});
	const cancel = () => {
		void ahp
			.extension("x-wingman/cancelSearch", { channel: "ahp-root://", searchId })
			.catch(() => {});
	};
	signal?.addEventListener("abort", cancel, { once: true });
	try {
		const result = await ahp.extension<WorkspaceSearchSummary>(
			"x-wingman/textSearch",
			{ channel: "ahp-root://", searchId, ...request },
		);
		signal?.throwIfAborted();
		return result;
	} finally {
		release();
		signal?.removeEventListener("abort", cancel);
	}
}
