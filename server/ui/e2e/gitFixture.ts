import type { Page } from "@playwright/test";
import { pathToFileURL } from "node:url";
import { join } from "node:path";
import { mockWorkspace } from "./workspaceFixture.ts";
import { mockResource } from "./ahpFixture.ts";

type Request = {
	params: Record<string, any>;
	fulfill(reply: {
		json?: any;
		error?: { code: number; message: string };
	}): Promise<void>;
};
type Handler = (request: Request) => Promise<void>;
const fixtures = new WeakMap<Page, Map<string, Handler>>();

/** Presentation fixtures use the same Git HTTP and AHP changeset wire shapes as the host. */
export async function mockGit(page: Page, method: string, run: Handler) {
	if (method === "status") {
		return mockWorkspace(page, async (route) =>
			run({
				params: {},
				fulfill: async ({ json }) => route.fulfill({ json: { git: json } }),
			}),
		);
	}
	let handlers = fixtures.get(page);
	if (!handlers) {
		handlers = new Map();
		fixtures.set(page, handlers);
		const states = new Map<string, any>();
		const contents = new Map<string, string>();
		await mockResource(page, "subscribe", async (request) => {
			const state = states.get(request.params.channel);
			if (!state) return request.continue();
			await request.fulfill({
				json: {
					snapshot: { resource: request.params.channel, state, fromSeq: 0 },
				},
			});
		});
		await mockResource(page, "unsubscribe", async (request) => {
			if (!states.has(request.params.channel)) return request.continue();
			await request.fulfill({ json: {} });
		});
		await mockResource(page, "resourceRead", async (request) => {
			const data = contents.get(request.params.uri);
			if (data === undefined) return request.continue();
			await request.fulfill({ json: { data, encoding: "utf-8" } });
		});
		await page.route(/\/api\/workspace\/git\//, async (route) => {
			const url = new URL(route.request().url());
			let operation = url.pathname.split("/").pop()!;
			if (operation === "fetch") operation = "branches";
			if (operation === "commits" && route.request().method() === "POST")
				operation = "commit";
			const handler = handlers!.get(operation);
			if (!handler) return route.continue();
			await handler({
				params:
					route.request().method() === "GET"
						? Object.fromEntries(url.searchParams)
						: (route.request().postDataJSON() ?? {}),
				fulfill: async ({ json, error }) => {
					if (operation === "diff" && json?.files) {
						const channel = `ahp-changeset:/wingman/compare/fixture-${states.size}`;
						const uri = (path: string) =>
							pathToFileURL(join(process.env.E2E_WORKSPACE!, path)).href;
						const content = (text: string) => {
							const ref = { uri: `ahp-content:/fixture-${contents.size}` };
							contents.set(ref.uri, text);
							return ref;
						};
						const { files, ...metadata } = json;
						states.set(channel, {
							status: "ready",
							files: files.map((file: any) => ({
								id: uri(file.path),
								nonce: "fixture",
								edit: {
									before:
										file.status === "added"
											? undefined
											: {
													uri: uri(file.original_path || file.path),
													content: content(file.original ?? ""),
												},
									after:
										file.status === "deleted"
											? undefined
											: {
													uri: uri(file.path),
													content: content(file.modified ?? ""),
												},
								},
								_meta: { wingman: { patch: content(file.patch) } },
							})),
						});
						json = { ...metadata, channel };
					}
					if (error)
						await route.fulfill({
							status: 409,
							contentType: "text/plain",
							body: error.message,
						});
					else await route.fulfill({ json });
				},
			});
		});
	}
	handlers.set(method, run);
}
