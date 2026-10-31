import { useMemo, useSyncExternalStore } from "react";
import type { AgentInfo } from "@microsoft/agent-host-protocol";
import { workspaceClient } from "../state/workspaceClient.ts";

/** The host's agent providers, as the AHP root lists them. */
export function useRootAgents(): AgentInfo[] {
	const client = workspaceClient();
	return useSyncExternalStore(
		(listener) => client.ahp.watch("ahp-root://", listener),
		() => client.ahp.root.agents,
	);
}

export function useAgents(): { id: string; name: string }[] {
	const agents = useRootAgents();
	return useMemo(
		() =>
			agents.map((agent) => ({ id: agent.provider, name: agent.displayName })),
		[agents],
	);
}
