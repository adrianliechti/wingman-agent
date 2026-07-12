import { Pencil, Play, X } from "lucide-react";
import type { PendingTurnInput } from "../hooks/useWebSocket";
import type { TurnInputState } from "../types/protocol";

interface Props {
	items: PendingTurnInput[];
	paused: boolean;
	onEdit: (item: PendingTurnInput) => void;
	onRemove?: (id: string, state: TurnInputState) => void;
	onResume?: () => void;
	onClear?: () => void;
}

function statusLabel(item: PendingTurnInput): string {
	switch (item.state) {
		case "queued":
			return item.position > 0 ? `#${item.position}` : "Queued";
		case "sending":
			return "Sending";
		case "failed":
			return "Failed";
		case "cancelled":
			return "Cancelled";
		default:
			return item.state;
	}
}

export function TurnQueue({ items, paused, onEdit, onRemove, onResume, onClear }: Props) {
	const queued = items.filter(
		(item) => item.state === "queued" || item.state === "sending",
	);
	return (
		<div className="mb-2 overflow-hidden rounded-lg border border-border-subtle bg-bg-surface/90 shadow-sm">
			<div className="flex h-7 items-center gap-2 border-b border-border-subtle px-2.5">
				<span className="text-[10px] font-mono uppercase tracking-wide text-fg-dim">
					{paused ? "Queue paused" : `${queued.length} queued`}
				</span>
				<div className="flex-1" />
				{paused && queued.length > 0 && (
					<button
						type="button"
						className="flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] text-success hover:bg-bg-hover"
						onClick={onResume}
						title="Run the next queued message"
					>
						<Play size={10} fill="currentColor" />
						Resume
					</button>
				)}
				{queued.length > 0 && (
					<button
						type="button"
						className="rounded px-1.5 py-0.5 text-[10px] text-fg-dim hover:bg-bg-hover hover:text-danger"
						onClick={onClear}
						title="Remove every queued message"
					>
						Clear
					</button>
				)}
			</div>
			<div className="max-h-28 overflow-y-auto overscroll-contain py-0.5">
				{items.map((item) => {
					const attachments = item.files.length + item.images.length;
					const editable =
						item.state === "queued" || item.state === "failed" || item.state === "cancelled";
					return (
						<div
							key={item.id}
							className="group flex min-w-0 items-center gap-2 px-2.5 py-1 hover:bg-bg-hover/60"
							title={item.error}
						>
							<span
								className={`w-14 shrink-0 text-[9px] font-mono uppercase ${
									item.state === "failed"
										? "text-danger"
										: item.state === "cancelled"
											? "text-fg-dim"
											: "text-warning"
								}`}
							>
								{statusLabel(item)}
							</span>
							<span className="min-w-0 flex-1 truncate text-[11px] font-mono text-fg-muted">
								{item.text ||
									(item.files.length > 0
										? "File context"
										: item.images.length > 0
											? "Image attachment"
											: "Empty message")}
							</span>
							{attachments > 0 && (
								<span className="shrink-0 text-[9px] font-mono text-fg-dim">+{attachments}</span>
							)}
							{editable && (
								<button
									type="button"
									className="flex h-5 w-5 shrink-0 items-center justify-center rounded text-fg-dim hover:bg-bg-active hover:text-fg"
									onClick={() => onEdit(item)}
									title={item.state === "queued" ? "Edit queued message" : "Restore as draft"}
								>
									<Pencil size={10} />
								</button>
							)}
							<button
								type="button"
								className="flex h-5 w-5 shrink-0 items-center justify-center rounded text-fg-dim hover:bg-bg-active hover:text-danger"
								onClick={() => onRemove?.(item.id, item.state)}
								title={item.state === "queued" || item.state === "sending" ? "Remove from queue" : "Dismiss"}
							>
								<X size={11} />
							</button>
						</div>
					);
				})}
			</div>
		</div>
	);
}
