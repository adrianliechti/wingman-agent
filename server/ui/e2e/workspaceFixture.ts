import type { Page } from "@playwright/test";

type Handler = (route: any) => unknown;
const handlers = new WeakMap<Page, Handler[]>();

/** Merge presentation overrides into real startup identity and host information. */
export async function mockWorkspace(page: Page, run: Handler) {
	let list = handlers.get(page);
	if (!list) {
		list = [];
		handlers.set(page, list);

		let gitOverride: any, backendsOverride: any;
		await page.route(/\/api\/workspace$/, async (route) => {
			let data = await (await route.fetch()).json();
			for (const handler of list!)
				await handler({
					fetch: async () => ({ json: async () => data }),
					fulfill: async ({ json }: any) => {
						data = { ...data, ...json };
					},
				});
			const { git, backends, ...workspace } = data;
			gitOverride = git;
			backendsOverride = backends;
			await route.fulfill({ json: workspace });
		});
		await page.route(/\/api\/workspace\/git$/, async (route) => {
			if (gitOverride === undefined) return route.continue();
			const actual =
				gitOverride === true ? await (await route.fetch()).json() : undefined;
			await route.fulfill({
				json:
					gitOverride === true
						? (actual ?? {
								branch: "main",
								head: "",
								ahead: 0,
								behind: 0,
								has_remote: false,
								files: [],
							})
						: gitOverride === false
							? null
							: gitOverride,
			});
		});
		await page.route(/\/api\/v2\/backends$/, async (route) => {
			if (backendsOverride === undefined) return route.continue();
			await route.fulfill({ json: backendsOverride });
		});
	}
	list.push(run);
}
