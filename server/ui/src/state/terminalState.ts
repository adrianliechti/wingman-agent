import type { TerminalState } from "@microsoft/agent-host-protocol";

// The host's retention limits, as in VS Code's agent host.
export const terminalMaxContentLength = 100_000;
export const terminalTrimmedContentLength = 80_000;

/** Bounds a reduced terminal state the way the host bounds its own. */
export function boundTerminalState(state: TerminalState): TerminalState {
	let size = 0;
	for (const part of state.content)
		size += part.type === "command" ? part.output.length : part.value.length;
	if (size <= terminalMaxContentLength) return state;
	const content = [...state.content];
	while (size > terminalTrimmedContentLength && content.length > 1) {
		const removed = content.shift()!;
		size -= removed.type === "command" ? removed.output.length : removed.value.length;
	}
	const excess = size - terminalTrimmedContentLength;
	if (excess > 0) {
		const head = content[0];
		content[0] =
			head.type === "command"
				? { ...head, output: head.output.slice(excess) }
				: { ...head, value: head.value.slice(excess) };
	}
	return { ...state, content };
}
