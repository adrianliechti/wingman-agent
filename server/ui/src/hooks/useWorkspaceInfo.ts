import { useQuery } from "@tanstack/react-query";
import { workspaceQuery, type WorkspaceInfo } from "../api/workspace";

export function useWorkspaceInfo(): WorkspaceInfo | null {
	return useQuery(workspaceQuery).data ?? null;
}
