import { workspaceURL } from "./http.ts";
import { queryOptions } from "@tanstack/react-query";
import type { FileContent } from "../types/protocol";
import { queryKeys } from "./query.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { resourceErrorCode } from "../state/fileClient.ts";

export interface FileHit {
	path: string;
	name: string;
}

export function workspaceFilePreviewURL(path: string): string {
	return workspaceURL(`/preview?path=${encodeURIComponent(path)}`);
}

export function workspaceFileDownloadURL(path: string): string {
	return workspaceURL(`/content?path=${encodeURIComponent(path)}`);
}

export function isWorkspaceFileConflict(error: unknown): boolean {
	return [-32010, -32011].includes(resourceErrorCode(error) ?? 0);
}

export interface CreateFileOptions {
	content?: string;
	directory?: boolean;
}

export interface WriteFileInput {
	path: string;
	content: string;
	revision?: string;
	force?: boolean;
}

export type WriteFileResult =
	| { ok: true; revision: string }
	| { ok: false; conflict: true; error: string };

export async function deleteWorkspaceFile(path: string): Promise<void> {
	await workspaceClient().files.delete(path);
}
export async function moveWorkspaceFile(
	from: string,
	to: string,
): Promise<void> {
	await workspaceClient().files.move(from, to);
}
export async function copyWorkspaceFile(
	from: string,
	to: string,
): Promise<void> {
	await workspaceClient().files.copy(from, to);
}
export function getAbsoluteWorkspacePath(path: string): Promise<string> {
	return workspaceClient().files.absolutePath(path);
}
export async function revealWorkspaceFile(path: string): Promise<void> {
	await workspaceClient().files.reveal(path);
}
export const fileQueries = {
	tree: (path: string) =>
		queryOptions({
			queryKey: queryKeys.files.tree(path),
			queryFn: ({ signal }) => workspaceClient().files.list(path, signal),
		}),
	search: (query: string) =>
		queryOptions({
			queryKey: queryKeys.files.search(query),
			queryFn: ({ signal }) => workspaceClient().files.search(query, signal),
		}),
};
export function readWorkspaceFile(
	path: string,
	external = false,
	signal?: AbortSignal,
): Promise<FileContent> {
	return workspaceClient().files.read(path, external, signal);
}
export function createWorkspaceFile(
	path: string,
	options: CreateFileOptions = {},
): Promise<FileContent | null> {
	return workspaceClient().files.create(path, options);
}
export async function writeWorkspaceFile(
	input: WriteFileInput,
): Promise<WriteFileResult> {
	try {
		return { ok: true, ...(await workspaceClient().files.write(input)) };
	} catch (error) {
		if (resourceErrorCode(error) === -32011)
			return {
				ok: false,
				conflict: true,
				error: error instanceof Error ? error.message : "File changed on disk.",
			};
		throw error;
	}
}
