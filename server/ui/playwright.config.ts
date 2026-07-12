import { defineConfig } from "@playwright/test";

export default defineConfig({
	testDir: "./e2e",
	timeout: 30_000,
	fullyParallel: false,
	workers: 1,
	reporter: "line",
	use: {
		baseURL: process.env.E2E_BASE_URL,
		headless: true,
		viewport: { width: 1280, height: 800 },
	},
});
