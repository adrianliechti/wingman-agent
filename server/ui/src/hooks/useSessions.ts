import type { MessageAttachment } from "@microsoft/agent-host-protocol";
import { useState, useSyncExternalStore } from "react";
import {
	workspaceClient,
	type WorkspaceClient,
} from "../state/workspaceClient.ts";
import type { TurnInputIntent } from "../types/protocol.ts";
import { useToast } from "../components/ui/Feedback.tsx";

// React bindings only. Transport, delivery receipts, and the ordered session
// store each have an owner outside the component lifecycle.
export function useSessions() {
	const client = workspaceClient();
	const views = useSyncExternalStore(
		client.store.subscribeRender,
		client.store.getSummaries,
	);
	const connected = useSyncExternalStore(
		client.subscribeConnection,
		client.getConnected,
	);
	const toast = useToast();
	const [dismissed, setDismissed] = useState<Record<string, string | null>>({});
	const sessions = Object.fromEntries(
		Object.entries(views).map(([key, view]) => [
			key,
			{ ...view, error: dismissed[key] === view.error ? null : view.error },
		]),
	);
	const act = async (
		operation: (client: WorkspaceClient) => Promise<unknown>,
	) => {
		try {
			await operation(client);
			return true;
		} catch (error) {
			toast({
				title: "Could not update session",
				description: String(error),
				tone: "error",
			});
			return false;
		}
	};
	return {
		connected,
		sessions,
		act,
		subscribe: client.subscribe,
		observe: (keys: string[]) => {
			const releases = keys.map((key) => client.watch(key));
			return () => releases.forEach((release) => release());
		},
		sendChat: async (
			key: string,
			text: string,
			files?: string[],
			images?: string[],
			intent: TurnInputIntent = "follow_up",
			completions?: MessageAttachment[],
		) => {
			try {
				await client.send(key, text, files, images, intent, completions);
				return true;
			} catch (error) {
				toast({
					title: "Input delivery was not confirmed",
					description: `${String(error)}. Your input is kept; retrying uses the same request.`,
					tone: "error",
				});
				return false;
			}
		},
		dismissError: (key: string) =>
			setDismissed((current) => ({
				...current,
				[key]: client.store.getSnapshot()[key]?.error,
			})),
	};
}
