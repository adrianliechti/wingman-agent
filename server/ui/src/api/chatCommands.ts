import { queryOptions } from "@tanstack/react-query";
import type {
	CompletionItem,
	MessageAttachment,
	SessionState,
} from "@microsoft/agent-host-protocol";
import { isDraft } from "../state/sessionStore.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { queryKeys } from "./query.ts";
import { listWorkspaceSkills, type Skill } from "./skills.ts";

export interface ChatCommand extends Skill {
	kind: "skill" | "command" | "action";
	attachment?: MessageAttachment;
}

export function completionCommand(
	item: CompletionItem,
): ChatCommand | undefined {
	const info = item.attachment._meta?.wingman as
		| Partial<ChatCommand>
		| undefined;
	if (
		!info ||
		typeof info.name !== "string" ||
		!["skill", "command"].includes(info.kind ?? "") ||
		item.insertText !== `/${info.name}`
	)
		return;
	return {
		name: info.name,
		kind: info.kind as "skill" | "command",
		description:
			typeof info.description === "string" ? info.description : undefined,
		input_hint:
			typeof info.input_hint === "string" ? info.input_hint : undefined,
		attachment: item.attachment,
	};
}

export function chatCommandsQuery(sessionId?: string, inline = false) {
	return queryOptions({
		queryKey:
			!sessionId || isDraft(sessionId)
				? queryKeys.skills.list
				: queryKeys.chatCommands.list(sessionId, inline),
		staleTime: 0,
		queryFn: async ({ signal }): Promise<ChatCommand[]> => {
			if (!sessionId || isDraft(sessionId)) {
				const skills = await listWorkspaceSkills(signal);
				return skills.map((skill) => ({ ...skill, kind: "skill" }));
			}
			const client = workspaceClient();
			const uri = await client.resource(sessionId);
			const session = await client.ahp.read<SessionState>(uri);
			if (!session.defaultChat) throw new Error("Session has no chat");
			signal.throwIfAborted();
			// Fetch a catalogue on opening the picker; keystroke filtering is local.
			// A leading space requests inline skills without native slash commands.
			const text = inline ? " /" : "/";
			const result = await client.ahp.request("completions", {
				channel: session.defaultChat,
				kind: "userMessage" as import("@microsoft/agent-host-protocol").CompletionItemKind.UserMessage,
				text,
				offset: text.length,
			});
			signal.throwIfAborted();
			return result.items.flatMap((item) => {
				const command = completionCommand(item);
				return command ? [command] : [];
			});
		},
	});
}

// Keep the original attachment (including all opaque metadata) only while its
// invocation is still in the draft. Instructions are resolved by the host.
export function selectedCommandAttachments(
	text: string,
	attachments: MessageAttachment[] = [],
): MessageAttachment[] {
	const names = new Set(
		[...text.matchAll(/(?:^|\s)(\/[^\s/]+)(?=\s|$)/g)].map((match) => match[1]),
	);
	return attachments.filter((attachment) => names.has(attachment.label));
}
