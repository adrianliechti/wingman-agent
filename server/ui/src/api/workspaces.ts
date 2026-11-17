import { fetchJSON, fetchOK } from "./http.ts";

export async function chooseWorkspaceFolder(): Promise<string | undefined> {
	const result = await fetchJSON<{ path?: string }>("/app/folder", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: "{}",
	});
	return result.path;
}

/** Opens a folder in the desktop app's host, beside the folders already open. */
export async function openWorkspace(path: string): Promise<void> {
	await fetchOK("/app/workspaces/open", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ path }),
	});
}
