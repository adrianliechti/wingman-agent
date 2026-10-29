import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { chatCommandsQuery, type ChatCommand } from "../api/chatCommands";
import { isDraft } from "../state/sessionStore.ts";

const EMPTY: ChatCommand[] = [];
export function useChatCommands(
	sessionId?: string,
	active = true,
	canCompact = false,
	inline = false,
): ChatCommand[] {
	const { data = EMPTY } = useQuery({
		...chatCommandsQuery(sessionId, inline),
		enabled: active,
	});
	return useMemo(() => {
		if (!canCompact || inline || !sessionId || isDraft(sessionId)) return data;
		return [
			...data.filter((item) => item.name !== "compact"),
			{
				name: "compact",
				kind: "action" as const,
				description: "Summarize the conversation to free context",
				input_hint: "[focus]",
			},
		];
	}, [data, sessionId, canCompact, inline]);
}
