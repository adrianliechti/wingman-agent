import { useCallback, useSyncExternalStore } from "react";
import { workspaceClient } from "../state/workspaceClient.ts";
export function useSessionView(key = "") {
	const store = workspaceClient().store;
	return useSyncExternalStore(
		store.subscribeRender,
		useCallback(() => store.getSnapshot()[key], [store, key]),
	);
}
