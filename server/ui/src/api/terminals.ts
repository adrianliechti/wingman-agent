import { queryOptions } from "@tanstack/react-query";
import type {
	TerminalState,
	TerminalClaim,
	TerminalInfo,
} from "@microsoft/agent-host-protocol";
import { RpcError } from "@microsoft/agent-host-protocol/client";
import type { TerminalEntry } from "../types/protocol";
import { queryKeys } from "./query.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { ActionType } from "../state/ahpValues.ts";

export const terminalURI = (id: string) =>
	id.startsWith("ahp-terminal:") ? id : `ahp-terminal:/wingman/${id}`;
export function terminalEntry(
	uri: string,
	state: TerminalInfo | TerminalState,
): TerminalEntry {
	return {
		id: uri,
		title: state.title,
		shell: "",
		cols: "cols" in state ? (state.cols ?? 80) : 80,
		rows: "rows" in state ? (state.rows ?? 24) : 24,
		busy: false,
	};
}
export function ownTerminalClaim(): TerminalClaim {
	return {
		kind: "client",
		clientId: workspaceClient().ahp.clientId,
	} as TerminalClaim;
}
export const terminalQueries = {
	list: () =>
		queryOptions({
			queryKey: queryKeys.terminals.list,
			queryFn: async () => {
				const ahp = workspaceClient().ahp;
				await ahp.ready();
				return (ahp.root.terminals ?? []).map((item) =>
					terminalEntry(item.resource, item),
				);
			},
		}),
};
export async function startTerminal(shell?: string): Promise<TerminalEntry> {
	const ahp = workspaceClient().ahp,
		uri = `ahp-terminal:/${crypto.randomUUID()}`;
	await ahp.request("createTerminal", {
		channel: uri,
		claim: ownTerminalClaim(),
		...(shell ? { _meta: { "wingman.shell": shell } } : {}),
	});
	return terminalEntry(uri, await ahp.read<TerminalState>(uri));
}
export async function getTerminal(id: string): Promise<TerminalEntry | null> {
	const uri = terminalURI(id);
	try {
		const ahp = workspaceClient().ahp;
		const [state, status] = await Promise.all([
			ahp.read<TerminalState>(uri),
			ahp.extension<{ busy: boolean }>("x-wingman/terminalStatus", {
				channel: uri,
			}),
		]);
		return { ...terminalEntry(uri, state), busy: status.busy };
	} catch (error) {
		if (error instanceof RpcError && error.code === -32008) return null;
		throw error;
	}
}
export async function deleteTerminal(id: string): Promise<void> {
	const ahp = workspaceClient().ahp,
		uri = terminalURI(id);
	try {
		const state = await ahp.read<TerminalState>(uri);
		if (state.claim.kind !== "client" || state.claim.clientId !== ahp.clientId)
			await ahp.dispatch(uri, {
				type: ActionType.TerminalClaimed,
				claim: ownTerminalClaim(),
			});
		await ahp.request("disposeTerminal", { channel: uri });
	} catch (error) {
		if (!(error instanceof RpcError && error.code === -32008)) throw error;
	}
}
