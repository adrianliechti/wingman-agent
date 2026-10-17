import type { SessionView } from "./sessionStore.ts";

export function summarizeSession(view: SessionView) {
	return {
		id: view.id,
		status: view.status,
		phase: view.phase,
		error: view.error,
		synchronized: view.synchronized,
		usage: view.usage,
		firstUser:
			view.entries.find((e) => e.type === "user" && e.content.trim())
				?.content ?? "",
		needsInput: view.prompts.length > 0,
		// A completion marker changes once per completed output, never per token.
		completed: view.phase === "idle" ? (view.entries.at(-1)?.id ?? "") : "",
	};
}
export type SessionSummary = ReturnType<typeof summarizeSession>;
