import { useCallback, useEffect, useState } from "react";
import type { ServerMessage } from "../types/protocol";

interface Capabilities {
	git: boolean;
	lsp: boolean;
	diffs: boolean;
	notice?: string;
}

type Subscribe = (handler: (msg: ServerMessage) => void) => () => void;

export function useCapabilities(subscribe?: Subscribe): Capabilities | null {
	const [caps, setCaps] = useState<Capabilities | null>(null);

	const load = useCallback(async () => {
		try {
			const res = await fetch("/api/capabilities");
			if (!res.ok) return;
			const data: Capabilities = await res.json();
			setCaps(data);
		} catch {}
	}, []);

	useEffect(() => {
		// eslint-disable-next-line react-hooks/set-state-in-effect -- standard data-load on mount
		load();
	}, [load]);

	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "capabilities_changed") {
				load();
			}
		});
	}, [subscribe, load]);

	return caps;
}
