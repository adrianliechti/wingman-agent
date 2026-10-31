import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Every route the workspace server serves outside the UI bundle.
export function proxyToServer(target: string) {
	const routes = ["/api", "/ahp", "/lsp", "/dap", "/content", "/preview"];
	return Object.fromEntries(
		routes.map((route) => [route, { target, ws: true }]),
	);
}

export default defineConfig({
	plugins: [react({ compiler: true }), tailwindcss()],
	build: {
		outDir: "../static",
		emptyOutDir: true,
		rolldownOptions: {
			output: {
				// Keep checked-in assets stable; Vite handles chunk splitting.
				entryFileNames: "assets/[name].js",
				chunkFileNames: "assets/[name].js",
				assetFileNames: "assets/[name].[ext]",
			},
		},
	},
	worker: {
		rolldownOptions: {
			output: {
				entryFileNames: "assets/workers/[name].js",
				chunkFileNames: "assets/workers/[name].js",
			},
		},
	},
	server: {
		proxy: proxyToServer("http://localhost:4242"),
	},
});
