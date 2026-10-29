import type { AHPConnection } from "./ahpConnection.ts";
import type { LSPClient } from "./lspClient.ts";
import type { FileContent, FileEntry } from "../types/protocol.ts";
import type { ContentEncoding } from "@microsoft/agent-host-protocol";

const rootChannel = "ahp-root://";
const excludedDirs = new Set([
	".git",
	".hg",
	".svn",
	"node_modules",
	"__pycache__",
	".venv",
	"vendor",
	"dist",
	"build",
	"target",
	".next",
	".cache",
]);
const excludedFiles = new Set([".ds_store", "thumbs.db"]);

export function resourceErrorCode(error: unknown): number | undefined {
	return typeof error === "object" && error !== null && "code" in error
		? Number(error.code)
		: undefined;
}
export async function contentRevision(text: string): Promise<string> {
	const bytes = new TextEncoder().encode(text);
	const hash = await crypto.subtle.digest("SHA-256", bytes);
	return Array.from(new Uint8Array(hash), (x) =>
		x.toString(16).padStart(2, "0"),
	).join("");
}
export class FileClient {
	readonly ahp: AHPConnection;
	readonly lsp: LSPClient;
	constructor(ahp: AHPConnection, lsp: LSPClient) {
		this.ahp = ahp;
		this.lsp = lsp;
	}
	async uri(path: string) {
		await this.ahp.ready();
		if (
			path.startsWith("/") ||
			path.split(/[\\/]/).includes("..") ||
			path.includes("\\")
		)
			throw new Error("Invalid workspace path");
		const root = this.ahp.directory.replace(/\/$/, "");
		return path && path !== "."
			? `${root}/${path.split("/").map(encodeURIComponent).join("/")}`
			: root;
	}
	async absolutePath(path: string) {
		const url = new URL(await this.uri(path));
		const decoded = decodeURIComponent(url.pathname);
		return /^\/[A-Za-z]:\//.test(decoded) ? decoded.slice(1) : decoded;
	}
	async list(path: string, signal?: AbortSignal): Promise<FileEntry[]> {
		signal?.throwIfAborted();
		const result = await this.ahp.request("resourceList", {
			channel: rootChannel,
			uri: await this.uri(path),
		});
		signal?.throwIfAborted();
		return result.entries
			.filter(
				(entry) =>
					!excludedFiles.has(entry.name.toLowerCase()) &&
					!(entry.type === "directory" && excludedDirs.has(entry.name)),
			)
			.map((entry) => ({
				name: entry.name,
				path: path && path !== "." ? `${path}/${entry.name}` : entry.name,
				is_dir: entry.type === "directory",
			}));
	}
	async read(
		path: string,
		external = false,
		signal?: AbortSignal,
	): Promise<FileContent> {
		signal?.throwIfAborted();
		if (external) {
			const uri = absoluteFileURI(path);
			const result = await this.lsp.connection.request<{ text: string }>(
				"workspace/textDocumentContent",
				{ uri },
				signal,
			);
			return textFile(path, result.text);
		}
		const uri = await this.uri(path);
		const metadata = await this.ahp.request("resourceResolve", {
			channel: rootChannel,
			uri,
		});
		signal?.throwIfAborted();
		if (metadata.type !== "file") throw new Error("Resource is not a file");
		const mime = metadata.contentType ?? "application/octet-stream";
		if (
			(metadata.size ?? 0) > 16 * 1024 * 1024 ||
			/^(image\/(?!svg)|audio\/|video\/|application\/(pdf|octet-stream))/.test(
				mime,
			)
		)
			return {
				path,
				binary: true,
				mime,
				size: metadata.size ?? 0,
				revision: metadata.etag ?? "",
			};
		const result = await this.ahp.request("resourceRead", {
			channel: rootChannel,
			uri,
		});
		signal?.throwIfAborted();
		if (result.encoding === "base64") {
			return {
				path,
				binary: true,
				mime: result.contentType,
				size:
					Math.floor((result.data.length * 3) / 4) -
					(result.data.endsWith("==") ? 2 : result.data.endsWith("=") ? 1 : 0),
				revision: metadata.etag ?? "",
			};
		}
		return textFile(path, result.data);
	}
	async create(
		path: string,
		options: { content?: string; directory?: boolean } = {},
	) {
		const uri = await this.uri(path);
		if (options.directory) {
			await this.ahp.request("resourceMkdir", { channel: rootChannel, uri });
			return null;
		}
		await this.ahp.request("resourceWrite", {
			channel: rootChannel,
			uri,
			encoding: "utf-8" as ContentEncoding,
			data: options.content ?? "",
			createOnly: true,
		});
		return options.content === undefined
			? null
			: textFile(path, options.content);
	}
	async write(input: {
		path: string;
		content: string;
		revision?: string;
		force?: boolean;
	}) {
		if (!input.force && !input.revision)
			throw new Error("File revision is required");
		await this.ahp.request("resourceWrite", {
			channel: rootChannel,
			uri: await this.uri(input.path),
			data: input.content,
			encoding: "utf-8" as ContentEncoding,
			...(!input.force ? { ifMatch: input.revision } : {}),
		});
		// Wingman's standard resource etags are content hashes, also used by search
		// and edit review. Derive this from the bytes saved, never from a later stat
		// which could already describe a concurrent writer's version.
		return { revision: await contentRevision(input.content) };
	}
	async delete(path: string) {
		try {
			await this.ahp.request("resourceDelete", {
				channel: rootChannel,
				uri: await this.uri(path),
				recursive: true,
			});
		} catch (error) {
			if (resourceErrorCode(error) !== -32008) throw error;
		}
	}
	async move(from: string, to: string) {
		await this.ahp.request("resourceMove", {
			channel: rootChannel,
			source: await this.uri(from),
			destination: await this.uri(to),
			failIfExists: true,
		});
	}
	async copy(from: string, to: string) {
		await this.ahp.request("resourceCopy", {
			channel: rootChannel,
			source: await this.uri(from),
			destination: await this.uri(to),
			failIfExists: true,
		});
	}
	async reveal(path: string) {
		await this.ahp.extension("x-wingman/revealResource", {
			channel: rootChannel,
			uri: await this.uri(path),
		});
	}
	async search(
		query: string,
		signal?: AbortSignal,
	): Promise<{ path: string; name: string }[]> {
		signal?.throwIfAborted();
		const result = await this.ahp.extension<{ path: string; name: string }[]>(
			"x-wingman/fileSearch",
			{ channel: rootChannel, query },
		);
		signal?.throwIfAborted();
		return result;
	}
}
export function absoluteFileURI(path: string) {
	const normalized = path.replaceAll("\\", "/");
	const pathname = /^[A-Za-z]:\//.test(normalized)
		? `/${normalized}`
		: normalized;
	return `file://${pathname.split("/").map(encodeURIComponent).join("/")}`;
}
async function textFile(path: string, content: string): Promise<FileContent> {
	return {
		path,
		content,
		language: languageForPath(path),
		revision: await contentRevision(content),
		size: new TextEncoder().encode(content).byteLength,
	};
}

const languages: Record<string, string> = {
	".go": "go",
	".js": "javascript",
	".ts": "typescript",
	".tsx": "typescript",
	".jsx": "javascript",
	".py": "python",
	".rs": "rust",
	".java": "java",
	".kt": "kotlin",
	".rb": "ruby",
	".php": "php",
	".c": "c",
	".cpp": "cpp",
	".h": "c",
	".hpp": "cpp",
	".cs": "csharp",
	".swift": "swift",
	".sh": "shell",
	".bash": "shell",
	".zsh": "shell",
	".ksh": "shell",
	".yaml": "yaml",
	".yml": "yaml",
	".json": "json",
	".csv": "plaintext",
	".tsv": "plaintext",
	".xml": "xml",
	".svg": "xml",
	".html": "html",
	".htm": "html",
	".css": "css",
	".scss": "scss",
	".sql": "sql",
	".md": "markdown",
	".markdown": "markdown",
	".mmd": "plaintext",
	".mermaid": "plaintext",
	".toml": "toml",
	".ini": "ini",
	".cfg": "ini",
	".dockerfile": "dockerfile",
	".proto": "protobuf",
	".lua": "lua",
	".r": "r",
	".dart": "dart",
	".zig": "zig",
	".ex": "elixir",
	".exs": "elixir",
	".erl": "erlang",
	".hs": "haskell",
	".ml": "ocaml",
	".tf": "hcl",
	".vue": "vue",
	".svelte": "svelte",
};
export function languageForPath(path: string): string {
	const name = path.split("/").at(-1)?.toLowerCase() ?? "";
	return (
		languages[name.slice(name.lastIndexOf("."))] ??
		(
			{
				dockerfile: "dockerfile",
				makefile: "makefile",
				"cmakelists.txt": "cmake",
				".bashrc": "shell",
				".bash_profile": "shell",
				".zshrc": "shell",
			} as Record<string, string>
		)[name] ??
		"plaintext"
	);
}
