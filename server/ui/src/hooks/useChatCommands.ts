import { useQuery } from "@tanstack/react-query";
import {
	chatCommandsQuery,
	isCompactCommand,
	type ChatCommand,
} from "../api/chatCommands";
import { isDraft } from "../state/sessionStore.ts";

const EMPTY: ChatCommand[] = [];
export function useChatCommands(
	sessionId?: string,
	active = true,
	inline = false,
): ChatCommand[] {
	const { data = EMPTY } = useQuery({
		...chatCommandsQuery(sessionId, inline),
		enabled: active,
	});
	return data;
}
export function useCanCompact(sessionId?: string): boolean {
	const { data = EMPTY } = useQuery({
		...chatCommandsQuery(sessionId),
		enabled: !!sessionId && !isDraft(sessionId),
		staleTime: 60_000,
	});
	return data.some(isCompactCommand);
}
