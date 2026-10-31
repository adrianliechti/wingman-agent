import type {
	LSPActivityStatus,
	WorkspaceDiagnostics,
} from "../types/protocol";
import { workspaceClient } from "../state/workspaceClient.ts";
import { fetchJSON } from "./http.ts";

export type LSPDocumentEvent = "open" | "change" | "save" | "close";

export function getWorkspaceDiagnostics(
	signal?: AbortSignal,
): Promise<WorkspaceDiagnostics> {
	return workspaceClient().lsp.workspaceDiagnostics(
		signal,
	) as Promise<WorkspaceDiagnostics>;
}

export function getLSPActivity(
	signal?: AbortSignal,
): Promise<LSPActivityStatus> {
	return fetchJSON<LSPActivityStatus>("/api/workspace/activity", { signal });
}

export async function syncLSPDocument(
	event: LSPDocumentEvent,
	path: string,
	content = "",
): Promise<void> {
	await workspaceClient().lsp.sync(event, path, content);
}
