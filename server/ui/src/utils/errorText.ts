/**
 * A failure as people read it. A host error reads "RPC error <code>: …", and
 * an agent's own JSON-RPC error that the host passed on arrives as JSON with
 * its words in `data.error`: the innermost message is the one to show.
 */
export function errorText(error: unknown): string {
	let text = error instanceof Error ? error.message : String(error);
	text = text.replace(/^RPC error -?\d+: /, "");
	for (let depth = 0; depth < 3; depth++) {
		let inner: unknown;
		try {
			const value = JSON.parse(text) as {
				message?: unknown;
				data?: { error?: unknown; message?: unknown };
			};
			inner = value?.data?.error ?? value?.data?.message ?? value?.message;
		} catch {
			break;
		}
		if (typeof inner !== "string" || !inner || inner === text) break;
		text = inner;
	}
	return text;
}
