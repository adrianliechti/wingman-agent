import type {
	ChangesetState,
	ChangesetFile,
	ContentRef,
	ChangesetOperationTargetKind,
} from "@microsoft/agent-host-protocol";
import type { AHPConnection } from "./ahpConnection.ts";
import { languageForPath, resourceErrorCode } from "./fileClient.ts";
import type { DiffEntry, DiffLayer } from "../types/protocol.ts";
import { unifiedDiff } from "./unifiedDiff.ts";

function isTextType(type: string): boolean {
	return /^text\/|[/+](json|xml|javascript|ecmascript|yaml|toml|x-sh)\b/.test(
		type,
	);
}

export function workspaceChangeset(layer?: DiffLayer) {
	return `ahp-changeset:/wingman/${layer || "uncommitted"}`;
}

/** Standard AHP file edits and operations, shared with other agent clients. */
export class ChangesetClient {
	readonly ahp: AHPConnection;
	private subscriptions = new Map<string, () => void>();
	constructor(ahp: AHPConnection) {
		this.ahp = ahp;
	}
	close() {
		for (const release of this.subscriptions.values()) release();
		this.subscriptions.clear();
	}
	/** Keeps a workspace layer subscribed until the client closes. */
	watch(channel: string) {
		if (!this.subscriptions.has(channel))
			this.subscriptions.set(
				channel,
				this.ahp.watch(channel, () => {}),
			);
	}
	private async state(channel: string): Promise<ChangesetState> {
		await this.ahp.ready();
		if (!channel.includes("/compare/")) this.watch(channel);
		const state = await this.ahp.read<ChangesetState>(channel);
		if (state.status === "error")
			throw new Error(state.error?.message ?? "Changes are unavailable");
		return state;
	}
	private relativePath(uri: string): string {
		const root = this.ahp.directory.replace(/\/$/, "") + "/";
		if (!uri.startsWith(root))
			throw new Error("Changeset contains a path outside the workspace");
		return decodeURIComponent(uri.slice(root.length));
	}
	private async text(
		ref: ContentRef | undefined,
		signal?: AbortSignal,
	): Promise<string> {
		if (!ref) return "";
		signal?.throwIfAborted();
		const result = await this.ahp.request("resourceRead", {
			channel: "ahp-root://",
			uri: ref.uri,
		});
		signal?.throwIfAborted();
		if (result.encoding === "base64")
			return new TextDecoder().decode(
				Uint8Array.from(atob(result.data), (char) => char.charCodeAt(0)),
			);
		return result.data;
	}
	/** Text content, or undefined when it is binary or cannot be read. */
	private async content(
		ref: ContentRef,
		signal?: AbortSignal,
	): Promise<string | undefined> {
		if (ref.contentType && !isTextType(ref.contentType)) return;
		try {
			const text = await this.text(ref, signal);
			return text.includes("\0") ? undefined : text;
		} catch (error) {
			const code = resourceErrorCode(error);
			// Stale snapshots are retried by the caller; transport errors surface.
			if (signal?.aborted || code === undefined || code === -32008) throw error;
			return;
		}
	}
	private async file(
		file: ChangesetFile,
		full: boolean,
		signal?: AbortSignal,
	): Promise<DiffEntry> {
		const { before, after } = file.edit;
		const path = this.relativePath(after?.uri ?? before?.uri ?? file.id);
		const originalPath = before ? this.relativePath(before.uri) : path;
		const [original, modified] = await Promise.all([
			before ? this.content(before.content, signal) : "",
			after ? this.content(after.content, signal) : "",
		]);
		const binary = original === undefined || modified === undefined;
		const patch = unifiedDiff(
			before ? { path: originalPath, content: original } : undefined,
			after ? { path, content: modified } : undefined,
			{ binary },
		);
		return {
			path,
			original_path: originalPath !== path ? originalPath : undefined,
			status: !before ? "added" : !after ? "deleted" : "modified",
			patch,
			original: full && !binary ? original : undefined,
			modified: full && !binary ? modified : undefined,
			language: languageForPath(path),
		};
	}
	async files(
		channel: string,
		path?: string,
		full = true,
		signal?: AbortSignal,
	): Promise<DiffEntry[]> {
		// Keep comparison snapshots subscribed while their content is read.
		await this.ahp.ready();
		const release = this.ahp.watch(channel, () => {});
		try {
			for (let attempt = 0; ; attempt++) {
				signal?.throwIfAborted();
				const state = await this.state(channel);
				const files = state.files.filter(
					(file) =>
						!path ||
						this.relativePath(
							file.edit.after?.uri ?? file.edit.before?.uri ?? file.id,
						) === path,
				);
				const result: DiffEntry[] = new Array(files.length);
				let index = 0;
				try {
					await Promise.all(
						Array.from({ length: Math.min(8, files.length) }, async () => {
							while (index < files.length) {
								const current = index++;
								result[current] = await this.file(files[current], full, signal);
							}
						}),
					);
					signal?.throwIfAborted();
					return result;
				} catch (error) {
					if (attempt || resourceErrorCode(error) !== -32008) throw error;
				}
			}
		} finally {
			release();
		}
	}
	async operate(
		channel: string,
		operationId: string,
		paths?: string[],
	): Promise<void> {
		const state = await this.state(channel);
		if (!state.operations?.some((operation) => operation.id === operationId))
			throw new Error(
				"This change operation is no longer available. Refresh and retry.",
			);
		if (!paths) {
			await this.ahp.request("invokeChangesetOperation", {
				channel,
				operationId,
			});
			return;
		}
		for (const path of paths) {
			const file = state.files.find(
				(file) => this.relativePath(file.id) === path,
			);
			if (!file)
				throw new Error(
					`The change to ${path} is no longer available. Refresh and retry.`,
				);
			await this.ahp.request("invokeChangesetOperation", {
				channel,
				operationId,
				target: {
					kind: "resource" as ChangesetOperationTargetKind.Resource,
					resource: file.id,
					nonce: file.nonce,
				},
			});
		}
	}
}
