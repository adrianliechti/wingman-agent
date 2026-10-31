import { queryOptions } from "@tanstack/react-query";
import type {
	CompletionItem,
	MessageAttachment,
} from "@microsoft/agent-host-protocol";
import { isDraft } from "../state/sessionStore.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { queryKeys } from "./query.ts";
import { listWorkspaceSkills, type Skill } from "./skills.ts";

export interface ChatCommand extends Skill {
	kind: "skill" | "command";
	attachment?: MessageAttachment;
}
/** The host offers `/compact` only when the chat's backend can compact. */
export function isCompactCommand(command: ChatCommand): boolean {
	return command.kind === "command" && command.name === "compact";
}

/** Reads VS Code's slash-command completion convention. */
export function completionCommand(
	item: CompletionItem,
): ChatCommand | undefined {
	const meta = item.attachment._meta;
	if (
		typeof meta?.command !== "string" ||
		item.insertText !== `/${meta.command}`
	)
		return;
	return {
		name: meta.command,
		kind: meta.isSkill === true ? "skill" : "command",
		description:
			typeof meta.description === "string" ? meta.description : undefined,
		input_hint:
			typeof meta.argumentHint === "string" ? meta.argumentHint : undefined,
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
			// Fetch a catalogue on opening the picker; keystroke filtering is local.
			// A leading space requests inline skills without native slash commands.
			const text = inline ? " /" : "/";
			const result = await client.ahp.request("completions", {
				channel: sessionId,
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
