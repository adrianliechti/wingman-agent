import type { Page } from "@playwright/test";

type Handler = (request: LanguageRequest) => Promise<void>;
type LanguageRequest = {
	params: any;
	document: { uri: string; text: string };
	presentation(): any;
	fulfill(response: {
		json?: any;
		status?: number;
		response?: unknown;
	}): Promise<void>;
	fetch(): Promise<{ json(): Promise<any> }>;
};
const fixtures = new WeakMap<Page, LanguageFixture>();

/** Deterministic editor responses over real LSP frames. Presentation helpers
 * keep large diagnostic/edit fixtures readable; synchronization and positions
 * are read from the actual LSP notifications, never from an HTTP shadow API. */
class LanguageFixture {
	handlers: { method: string | RegExp; handler: Handler }[] = [];
	listeners = new Set<(method: string) => void>();
	async install(page: Page) {
		await page.routeWebSocket(/\/lsp(?:\?|$)/, (socket) => {
			const server = socket.connectToServer();
			const documents = new Map<string, string>();
			const pending = new Map<
				number,
				{ method: string; resolve?: (result: any) => void }
			>();
			const applications = new Map<string, () => void>();
			let applySequence = 0,
				root = "";
			server.onMessage((wire) => {
				const message = JSON.parse(String(wire)),
					entry = pending.get(message.id);
				if (entry?.method === "initialize")
					root =
						message.result?.capabilities?.experimental?.wingman?.workspaceURI ??
						"";
				if (entry) {
					pending.delete(message.id);
					for (const fn of this.listeners) fn(entry.method);
				}
				if (entry?.resolve) entry.resolve(message.result);
				else socket.send(wire);
			});
			socket.onMessage(async (wire) => {
				const message = JSON.parse(String(wire)),
					method = message.method,
					p = message.params ?? {};
				if (!method) {
					applications.get(message.id)?.();
					applications.delete(message.id);
					return;
				}
				const uri = p.textDocument?.uri ?? "";
				if (method === "textDocument/didOpen")
					documents.set(uri, p.textDocument.text);
				if (method === "textDocument/didChange")
					for (const change of p.contentChanges)
						documents.set(uri, change.text);
				if (method === "textDocument/didClose") documents.delete(uri);
				const match = this.handlers.findLast(({ method: candidate }) =>
					typeof candidate === "string"
						? candidate === method
						: candidate.test(method),
				);
				if (!match) {
					if (message.id !== undefined) pending.set(message.id, { method });
					server.send(wire);
					return;
				}
				const path = decodeURIComponent(
					uri.slice(root.replace(/\/$/, "").length + 1),
				);
				const text = documents.get(uri) ?? "";
				await match.handler({
					params: p,
					document: { uri, text },
					presentation: () => ({
						path,
						content: p.text ?? text,
						event: method.replace("textDocument/did", "").toLowerCase(),
						only: p.context?.only,
						trigger_kind: p.context?.triggerKind,
						line: p.position?.line + 1,
						column: p.position?.character + 1,
					}),
					fetch: () =>
						new Promise((resolve) => {
							pending.set(message.id, {
								method,
								resolve: (result) => resolve({ json: async () => result }),
							});
							server.send(wire);
						}),
					fulfill: async ({ json: result, status }) => {
						if (message.id === undefined) {
							server.send(wire);
							return;
						}
						if (status && status >= 400) {
							socket.send(
								JSON.stringify({
									jsonrpc: "2.0",
									id: message.id,
									error: { code: -32603, message: "Fixture request failed" },
								}),
							);
							return;
						}
						if (method === "textDocument/diagnostic")
							result = { kind: "full", items: (result ?? []).map(diagnostic) };
						if (method === "workspace/diagnostic") {
							const groups = Map.groupBy(
								result.diagnostics ?? [],
								(item: any) => item.path,
							);
							result = {
								items: [...groups].map(([path, items]) => ({
									uri: `${root}/${path}`,
									version: null,
									kind: "full",
									items: items.map(diagnostic),
								})),
								_wingman: result,
							};
						}
						if (method === "textDocument/codeAction")
							result = result.actions.map((action: any) => ({
								...action,
								...(action.edit
									? {
											edit: {
												...action.edit,
												_wingman: { documents: result.documents },
											},
										}
									: {}),
							}));
						if (method === "workspace/executeCommand") {
							for (const requested of result.edits) {
								const id = `fixture-apply:${++applySequence}`;
								const completed = new Promise<void>((resolve) =>
									applications.set(id, resolve),
								);
								socket.send(
									JSON.stringify({
										jsonrpc: "2.0",
										id,
										method: "workspace/applyEdit",
										params: {
											label: requested.label,
											edit: {
												...requested.edit,
												_wingman: { documents: requested.documents },
											},
											_wingmanRequestId: message.id,
										},
									}),
								);
								await completed;
							}
							result = null;
						}
						socket.send(
							JSON.stringify({ jsonrpc: "2.0", id: message.id, result }),
						);
						for (const fn of this.listeners) fn(method);
					},
				});
			});
		});
	}
}
async function fixture(page: Page) {
	let result = fixtures.get(page);
	if (!result) {
		result = new LanguageFixture();
		fixtures.set(page, result);
		await result.install(page);
	}
	return result;
}
export async function mockLanguage(
	page: Page,
	method: string | RegExp,
	handler: Handler,
) {
	(await fixture(page)).handlers.push({ method, handler });
}
export async function waitLanguageResult(page: Page, method: string) {
	const f = await fixture(page);
	return new Promise<void>((resolve) => {
		const listener = (received: string) => {
			if (received === method) {
				f.listeners.delete(listener);
				resolve();
			}
		};
		f.listeners.add(listener);
	});
}

export async function probeLanguageCapabilities(path: string): Promise<any> {
	const url = new URL("/lsp", process.env.E2E_BASE_URL!);
	url.protocol = "ws:";
	url.searchParams.set("instance", process.env.E2E_INSTANCE!);
	const socket = new WebSocket(url);
	try {
		return await new Promise((resolve, reject) => {
			const timer = setTimeout(() => {
				socket.close();
				reject(new Error("Language probe timed out"));
			}, 10000);
			socket.onerror = () => {
				clearTimeout(timer);
				reject(new Error("Language probe failed"));
			};
			socket.onopen = () =>
				socket.send(
					JSON.stringify({
						jsonrpc: "2.0",
						id: 1,
						method: "initialize",
						params: { processId: null, capabilities: {} },
					}),
				);
			socket.onmessage = (event) => {
				const message = JSON.parse(String(event.data));
				if (message.error) {
					clearTimeout(timer);
					reject(new Error(message.error.message));
					return;
				}
				if (message.id === 1) {
					const root =
						message.result.capabilities.experimental.wingman.workspaceURI;
					socket.send(
						JSON.stringify({
							jsonrpc: "2.0",
							method: "initialized",
							params: {},
						}),
					);
					socket.send(
						JSON.stringify({
							jsonrpc: "2.0",
							id: 2,
							method: "wingman/documentCapabilities",
							params: { textDocument: { uri: `${root}/${path}` } },
						}),
					);
				} else if (message.id === 2) {
					clearTimeout(timer);
					resolve(message.result);
				}
			};
		});
	} finally {
		socket.close();
	}
}
function diagnostic(item: any) {
	return {
		range: {
			start: { line: item.line - 1, character: item.column - 1 },
			end: {
				line: (item.end_line ?? item.line) - 1,
				character: (item.end_column ?? item.column + 1) - 1,
			},
		},
		message: item.message,
		source: item.source,
		severity:
			item.severity === "error" ? 1 : item.severity === "warning" ? 2 : 3,
	};
}
