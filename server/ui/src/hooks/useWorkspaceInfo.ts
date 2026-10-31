import { useQuery } from "@tanstack/react-query";
import { workspaceQuery, type WorkspaceInfo } from "../api/workspace";

export function useWorkspaceInfo(): WorkspaceInfo | null {
	return (
		useQuery({
			...workspaceQuery,
			// The workspace-changed broadcast is the primary signal; polling only
			// backstops a dropped socket while installs are running.
			refetchInterval: (query) =>
				query.state.data?.managed_tools?.state === "installing" ? 2000 : false,
		}).data ?? null
	);
}
