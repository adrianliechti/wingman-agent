import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

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
		proxy: {
			"/api": { target: "http://localhost:4242", ws: true },
		},
	},
});
