import { queryOptions } from "@tanstack/react-query";
import { queryKeys } from "./query.ts";
import { fetchJSON, APIError } from "./http.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { workspaceChangeset } from "../state/changesetClient.ts";
import type {
	CompareMode,
	GitBranches,
	GitCommit,
	GitCompare,
	GitStatus,
} from "../types/protocol";

export type GitRequestError = Error & { diagnosticContext: string };
export type GitCommand =
	| { type: "create_branch"; name: string }
	| { type: "checkout_branch"; name: string; remote?: string }
	| { type: "commit"; message: string }
	| { type: "pull" }
	| { type: "push" }
	| { type: "stage"; paths: string[] }
	| { type: "stage_all" }
	| { type: "unstage"; paths: string[] };
export type GitCommandType = GitCommand["type"];

export const gitStatusQuery = queryOptions({
	queryKey: queryKeys.git.status,
	queryFn: ({ signal }) =>
		fetchJSON<GitStatus | null>("/api/workspace/git", { signal }),
});

export function getGitHistory(signal?: AbortSignal): Promise<GitCommit[]> {
	return fetchJSON("/api/workspace/git/commits", { signal });
}
export function getGitBranches(signal?: AbortSignal): Promise<GitBranches> {
	return fetchJSON("/api/workspace/git/branches", { signal });
}
export function fetchGitBranches(): Promise<GitBranches> {
	return fetchJSON("/api/workspace/git/fetch", { method: "POST" });
}
export async function runGitCommand(
	command: GitCommand,
): Promise<string | undefined> {
	const client = workspaceClient();
	switch (command.type) {
		case "stage":
		case "stage_all":
			await client.changesets.operate(
				workspaceChangeset("unstaged"),
				"stage",
				command.type === "stage" ? command.paths : undefined,
			);
			return;
		case "unstage":
			await client.changesets.operate(
				workspaceChangeset("staged"),
				"unstage",
				command.paths,
			);
			return;
	}
	const { type, ...params } = command;
	const path =
		type === "create_branch"
			? "branches"
			: type === "checkout_branch"
				? "checkout"
				: type === "commit"
					? "commits"
					: type;
	const result = await fetchJSON<{ output: string }>(
		`/api/workspace/git/${path}`,
		{
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify(params),
		},
	);
	return result.output || undefined;
}
export async function initializeGitRepository(): Promise<void> {
	await fetchJSON("/api/workspace/git/init", { method: "POST" });
}
export async function fetchGitComparison(
	base: string,
	head: string,
	mode: CompareMode,
	signal?: AbortSignal,
): Promise<GitCompare> {
	const context = [
		`Operation: Compare Git revisions`,
		`Request: GET /api/workspace/git/diff`,
		`Base: ${base}`,
		`Target: ${head}`,
		`Mode: ${mode}`,
	].join("\n");
	try {
		const client = workspaceClient();
		const result = await fetchJSON<
			Omit<GitCompare, "files"> & { channel: string }
		>(`/api/workspace/git/diff?${new URLSearchParams({ base, head, mode })}`, {
			signal,
		});
		const files = await client.changesets.files(
			result.channel,
			undefined,
			false,
			signal,
		);
		return { ...result, files };
	} catch (value) {
		if (signal?.aborted) throw value;
		const error = (
			value instanceof Error ? value : new Error(String(value))
		) as GitRequestError;
		error.diagnosticContext =
			context +
			(error instanceof APIError ? `\nResponse: HTTP ${error.status}` : "") +
			"\n\nServer response:\n" +
			error.message;
		console.error("Git comparison failed:", error, error.diagnosticContext);
		throw error;
	}
}
