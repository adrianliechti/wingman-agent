import type * as monaco from "monaco-editor";
import {
	initializeWorkspace,
	fetchBootstrap,
} from "./state/workspaceClient.ts";
import { WorkspaceProvider } from "./state/WorkspaceProvider.tsx";
import { initializeComposerDrafts } from "./state/composerDrafts.ts";
import { QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./devicon-slim.css";
import "./index.css";
import App from "./App.tsx";
import { createServerQueryClient } from "./api/query.ts";
import { AppCrashed } from "./AppCrashed.tsx";
import { ErrorBoundary } from "./components/ErrorBoundary.tsx";
import { ToastProvider } from "./components/ui/Feedback.tsx";

// monaco-editor declares MonacoEnvironment inside its own module, so the name
// never reaches the global scope we assign it on.
declare global {
	interface Window {
		MonacoEnvironment?: monaco.Environment;
		shell?: Readonly<{
			platform: "macos" | "windows";
			titleBar: Readonly<{
				overlay: boolean;
				height: number;
				insets: Readonly<{ left: number; right: number }>;
				maximized: boolean;
			}>;
		}>;
	}
}

const root = createRoot(document.getElementById("root")!);
fetchBootstrap()
	.then(async (scope) => {
		initializeWorkspace(scope);
		await initializeComposerDrafts(scope.workspaceId);
		const queryClient = createServerQueryClient();
		root.render(
			<StrictMode>
				<QueryClientProvider client={queryClient}>
					<ErrorBoundary
						fallback={(error, _reset, errorInfo) => (
							<AppCrashed error={error} errorInfo={errorInfo} />
						)}
					>
						<ToastProvider>
							<WorkspaceProvider>
								<App />
							</WorkspaceProvider>
						</ToastProvider>
					</ErrorBoundary>
				</QueryClientProvider>
			</StrictMode>,
		);
	})
	.catch((error) => root.render(<AppCrashed error={error} errorInfo={null} />));
