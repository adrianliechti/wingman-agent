import { useState, useSyncExternalStore, type ReactNode } from "react";
import { WorkspaceContext, type SettingsPatch } from "./workspaceContext.ts";
import { workspaceClient } from "./workspaceClient.ts";
import { useRootAgents } from "../hooks/useAgents.ts";

export function WorkspaceProvider({ children }: { children: ReactNode }) {
	const client = workspaceClient();
	const replaced = useSyncExternalStore(
		client.subscribeConnection,
		client.getReplaced,
	);
	const agents = useRootAgents();
	const [selected, selectBackend] = useState(() =>
		decodeURIComponent(
			location.pathname.split("/").filter(Boolean)[0] ?? "wingman",
		),
	);
	// The root lists providers only once connected.
	const backend =
		agents.length && !agents.some((agent) => agent.provider === selected)
			? "wingman"
			: selected;
	const [drafts, setDrafts] = useState<Record<string, SettingsPatch>>({});
	const setDraft = (key: string, patch: SettingsPatch) =>
		setDrafts((current) => ({
			...current,
			[key]: { ...current[key], ...patch },
		}));
	// initializeWorkspace owns the connection. Stopping it on unmount would let
	// StrictMode's remount fail in-flight reads.
	const value = { backend, selectBackend, drafts, setDraft };
	return (
		<WorkspaceContext value={value}>
			{children}
			{replaced && (
				<div
					role="alert"
					className="fixed bottom-4 left-1/2 z-[150] flex max-w-lg -translate-x-1/2 items-center gap-4 rounded-lg border border-border bg-bg-elevated p-4 text-sm text-fg shadow-xl"
				>
					<span>
						This workspace was reopened. Copy any unsaved edits before
						reloading.
					</span>
					<button
						type="button"
						className="cursor-pointer rounded border border-border px-3 py-1.5"
						onClick={() => location.assign("/")}
					>
						Reload workspace
					</button>
				</div>
			)}
		</WorkspaceContext>
	);
}
