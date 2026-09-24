import { useRef, useState } from "react";

export function IncompleteTurnNotice({
	disabled,
	onContinue,
}: {
	disabled: boolean;
	onContinue: () => boolean | Promise<boolean>;
}) {
	const pending = useRef(false);
	const [sending, setSending] = useState(false);
	const [error, setError] = useState<string | null>(null);
	return (
		<div
			role="status"
			className="mb-1.5 flex items-start gap-2 rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted"
		>
			<span className="min-w-0 flex-1">
				Turn incomplete. Your output is saved.
				{error && <span className="block text-danger">{error}</span>}
			</span>
			<button
				type="button"
				className="shrink-0 text-fg hover:underline disabled:opacity-50"
				disabled={disabled || sending}
				onClick={async () => {
					if (pending.current) return;
					pending.current = true;
					setSending(true);
					setError(null);
					try {
						if (!(await onContinue()))
							setError("Could not continue. Try again.");
					} catch (error) {
						setError(error instanceof Error ? error.message : String(error));
					} finally {
						pending.current = false;
						setSending(false);
					}
				}}
			>
				{sending ? "Continuing…" : "Continue"}
			</button>
		</div>
	);
}
