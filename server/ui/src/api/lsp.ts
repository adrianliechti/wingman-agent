import { workspaceClient } from "../state/workspaceClient.ts";

export type LSPDocumentEvent = "open" | "change" | "save" | "close";

export async function syncLSPDocument(
	event: LSPDocumentEvent,
	path: string,
	content = "",
): Promise<void> {
	await workspaceClient().lsp.sync(event, path, content);
}
