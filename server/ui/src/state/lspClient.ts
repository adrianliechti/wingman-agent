import { EditorConnection } from "./editorConnection.ts";

/** Read-only documents outside the workspace that a language server reported. */
export const lspSourceScheme = "wingman-source";

/** One language connection multiplexes the workspace's language services. */
export class LSPClient {
	readonly connection: EditorConnection;
	private root = "";
	semanticTokensLegend = {
		tokenTypes: [] as string[],
		tokenModifiers: [] as string[],
	};
	private documents = new Map<string, { text: string; version: number }>();
	constructor(instance: string) {
		this.connection = new EditorConnection("lsp", instance, {
			processId: null,
			clientInfo: { name: "Wingman Web" },
			rootUri: null,
			capabilities: {
				general: { positionEncodings: ["utf-16"] },
				workspace: {
					applyEdit: true,
					workspaceEdit: {
						documentChanges: true,
						changeAnnotationSupport: {},
					},
					textDocumentContent: {},
					diagnostics: { refreshSupport: true },
				},
				textDocument: {
					synchronization: { didSave: true },
					completion: {
						completionItem: {
							snippetSupport: true,
							insertReplaceSupport: true,
						},
					},
					hover: { contentFormat: ["markdown", "plaintext"] },
					documentSymbol: { hierarchicalDocumentSymbolSupport: true },
					rename: { prepareSupport: true },
					codeAction: {
						codeActionLiteralSupport: {
							codeActionKind: {
								valueSet: ["", "quickfix", "refactor", "source"],
							},
						},
						isPreferredSupport: true,
						disabledSupport: true,
						dataSupport: true,
						resolveSupport: { properties: ["edit"] },
					},
					diagnostic: {},
					semanticTokens: { requests: { full: true } },
				},
			},
		});
		this.connection.onReady = (result) => {
			this.root =
				result.capabilities.experimental.wingman.workspaceURI.replace(
					/\/$/,
					"",
				) + "/";
			this.semanticTokensLegend =
				result.capabilities.semanticTokensProvider?.legend ??
				this.semanticTokensLegend;
			for (const [path, doc] of this.documents) this.open(path, doc);
		};
	}
	close() {
		this.connection.close();
	}
	async uri(path: string) {
		await this.connection.ready();
		return this.documentURI(path);
	}
	private documentURI(path: string) {
		return this.root + path.split("/").map(encodeURIComponent).join("/");
	}
	private open(path: string, doc: { text: string; version: number }) {
		const extensions: Record<string, string> = {
			ts: "typescript",
			tsx: "typescriptreact",
			js: "javascript",
			jsx: "javascriptreact",
			py: "python",
			rs: "rust",
			go: "go",
			java: "java",
			cs: "csharp",
		};
		this.connection.notify("textDocument/didOpen", {
			textDocument: {
				uri: this.documentURI(path),
				languageId: extensions[path.split(".").at(-1) ?? ""] ?? "plaintext",
				...doc,
			},
		});
	}
	async sync(
		event: "open" | "change" | "save" | "close",
		path: string,
		text = "",
	) {
		await this.connection.ready();
		const uri = this.documentURI(path),
			previous = this.documents.get(path);
		if (event === "close") {
			if (previous) {
				this.documents.delete(path);
				this.connection.notify("textDocument/didClose", {
					textDocument: { uri },
				});
			}
			return;
		}
		if (!previous) {
			const doc = { text, version: 1 };
			this.documents.set(path, doc);
			this.open(path, doc);
		} else if (text !== previous.text) {
			const doc = { text, version: previous.version + 1 };
			this.documents.set(path, doc);
			this.connection.notify("textDocument/didChange", {
				textDocument: { uri, version: doc.version },
				contentChanges: [{ text }],
			});
		}
		if (event === "save")
			this.connection.notify("textDocument/didSave", {
				textDocument: { uri },
				text,
			});
	}
	async feature<T>(
		method: string,
		body: Record<string, any>,
		signal?: AbortSignal,
		applyEdit?: (params: any) => Promise<boolean>,
	): Promise<T> {
		if (typeof body.content === "string")
			await this.sync("change", body.path, body.content);
		const textDocument = { uri: await this.uri(body.path) };
		let params: any = { textDocument };
		if (body.line !== undefined)
			params.position = { line: body.line - 1, character: body.column - 1 };
		if (body.range) params.range = body.range;
		if (body.options) params.options = body.options;
		if (body.new_name) params.newName = body.new_name;
		if (body.character) params.ch = body.character;
		if (body.trigger_kind)
			params.context = {
				triggerKind: body.trigger_kind,
				triggerCharacter: body.trigger_character || undefined,
				isRetrigger: body.is_retrigger ?? false,
			};
		if (method === "textDocument/references")
			params.context = { includeDeclaration: true };
		if (method === "textDocument/codeAction")
			params.context = {
				diagnostics: [],
				only: body.only,
				triggerKind: body.trigger_kind,
			};
		if (method === "completionItem/resolve") params = body.item;
		if (method === "codeAction/resolve") params = body.action;
		if (method === "workspace/executeCommand")
			Object.assign(params, body.command);
		const value = await this.connection.request<any>(method, params, signal, {
			applyEdit,
			// Command edits wait for the user's review; the host bounds the native call.
			timeout: method === "workspace/executeCommand" ? 0 : undefined,
		});
		// Only presentation adaptation lives here; wire values remain LSP values.
		if (method === "textDocument/hover")
			return { contents: value?.contents?.value ?? value?.contents ?? "" } as T;
		if (method === "textDocument/diagnostic")
			return diagnostics(body.path, value.items) as T;
		if (method === "textDocument/rename") return editEnvelope(value) as T;
		if (method === "textDocument/codeAction")
			return {
				actions: value ?? [],
				documents: Object.assign(
					{},
					...(value ?? []).map(
						(action: any) => action.edit?._wingman?.documents ?? {},
					),
				),
			} as T;
		if (method === "codeAction/resolve")
			return {
				action: value,
				documents: value?.edit?._wingman?.documents ?? {},
			} as T;
		return value as T;
	}
	async workspaceDiagnostics(signal?: AbortSignal) {
		const result = await this.connection.request<any>(
			"workspace/diagnostic",
			{},
			signal,
		);
		const items = result.items.flatMap((item: any) =>
			diagnostics(
				decodeURIComponent(item.uri.slice(this.root.length)),
				item.items,
			),
		);
		items.sort(
			(a: any, b: any) =>
				({ error: 0, warning: 1, info: 2 })[a.severity as "error"] -
					{ error: 0, warning: 1, info: 2 }[b.severity as "error"] ||
				a.path.localeCompare(b.path) ||
				a.line - b.line,
		);
		return {
			...result._wingman,
			unavailable_servers: result._wingman?.unavailable_servers ?? [],
			diagnostics: items,
		};
	}
}

export function editEnvelope(edit: any) {
	return { edit, documents: edit?._wingman?.documents ?? {} };
}
function diagnostics(path: string, items: any[]) {
	return items.map((item) => ({
		path,
		line: item.range.start.line + 1,
		column: item.range.start.character + 1,
		end_line: item.range.end.line + 1,
		end_column: item.range.end.character + 1,
		severity:
			item.severity === 1 ? "error" : item.severity === 2 ? "warning" : "info",
		message: item.message,
		source: item.source,
	}));
}
