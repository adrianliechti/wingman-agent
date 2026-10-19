import type { ChatEntry } from "../../types/session";

export interface Turn {
	key: string;
	user: ChatEntry | null;
	working: ChatEntry[];
	reply: ChatEntry[];
}

export function buildTurns(entries: ChatEntry[]): Turn[] {
	const turns: Turn[] = [];
	let counter = 0;

	for (const e of entries) {
		if (e.type === "user" || turns.length === 0) {
			turns.push({
				key: e.type === "user" ? e.id : `turn-${counter++}`,
				user: e.type === "user" ? e : null,
				working: [],
				reply: [],
			});
			if (e.type === "user") continue;
		}
		const t = turns[turns.length - 1];
		if (e.type === "assistant") {
			t.reply.push(e);
		} else {
			// Text followed by more work is progress; consecutive trailing
			// assistant messages are all parts of the visible answer.
			t.working.push(...t.reply, e);
			t.reply = [];
		}
	}

	return turns;
}

export function findEntryElement(
	root: HTMLElement,
	id: string,
): HTMLElement | null {
	for (const el of root.querySelectorAll<HTMLElement>("[data-entry-id]")) {
		if (el.dataset.entryId === id) return el;
	}
	return null;
}
