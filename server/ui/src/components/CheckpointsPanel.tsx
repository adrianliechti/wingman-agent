import { ClipboardCopy, History, RotateCcw } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import type { CheckpointEntry, ServerMessage } from "../types/protocol";

interface Props {
	subscribe?: (handler: (msg: ServerMessage) => void) => () => void;
}

interface MenuState {
	x: number;
	y: number;
	checkpoint: CheckpointEntry;
}

export function CheckpointsPanel({ subscribe }: Props) {
	const [checkpoints, setCheckpoints] = useState<CheckpointEntry[]>([]);
	const [menu, setMenu] = useState<MenuState | null>(null);

	const load = useCallback(async () => {
		try {
			const res = await fetch("/api/checkpoints");
			if (!res.ok) {
				setCheckpoints([]);
				return;
			}
			const data: CheckpointEntry[] = await res.json();
			setCheckpoints(data);
		} catch {
			setCheckpoints([]);
		}
	}, []);

	useEffect(() => {
		// eslint-disable-next-line react-hooks/set-state-in-effect -- standard data-load on mount
		load();
	}, [load]);

	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "checkpoints_changed") {
				load();
			}
		});
	}, [subscribe, load]);

	useEffect(() => {
		if (!menu) return;
		const close = () => setMenu(null);
		const onKey = (e: KeyboardEvent) => {
			if (e.key === "Escape") close();
		};
		document.addEventListener("mousedown", close);
		document.addEventListener("scroll", close, true);
		document.addEventListener("keydown", onKey);
		return () => {
			document.removeEventListener("mousedown", close);
			document.removeEventListener("scroll", close, true);
			document.removeEventListener("keydown", onKey);
		};
	}, [menu]);

	const handleRestore = async (cp: CheckpointEntry) => {
		setMenu(null);
		const ok = window.confirm(
			`Restore working tree to "${cp.message}"?\n\nThis will overwrite uncommitted changes.`,
		);
		if (!ok) return;
		await fetch(`/api/checkpoints/${encodeURIComponent(cp.hash)}/restore`, {
			method: "POST",
		});
	};

	const handleCopyHash = (cp: CheckpointEntry) => {
		setMenu(null);
		navigator.clipboard
			.write([
				new ClipboardItem({
					"text/plain": new Blob([cp.hash], { type: "text/plain" }),
				}),
			])
			.catch((e) =>
				alert(`Copy failed: ${e instanceof Error ? e.message : String(e)}`),
			);
	};

	return (
		<div className="flex flex-col h-full overflow-hidden bg-bg relative">
			<div className="h-8 px-3 flex items-center shrink-0">
				<span className="text-[11px] text-fg-muted">Checkpoints</span>
			</div>
			<div className="overflow-y-auto flex-1 px-1 pb-2">
				{checkpoints.length === 0 && (
					<div className="px-3 py-6 text-[11px] text-fg-dim text-center">
						No checkpoints yet. A checkpoint is created after each agent turn.
					</div>
				)}
				{checkpoints.map((cp, i) => {
					const isLatest = i === 0;
					return (
						<div
							key={cp.hash}
							className="flex items-center gap-2 mx-1 px-2 py-1.5 rounded text-[12px] text-fg-muted hover:bg-bg-hover hover:text-fg transition-colors cursor-default"
							onContextMenu={(e) => {
								e.preventDefault();
								e.stopPropagation();
								setMenu({ x: e.clientX, y: e.clientY, checkpoint: cp });
							}}
							title={cp.hash}
						>
							<History
								size={11}
								className={isLatest ? "text-accent" : "text-fg-dim shrink-0"}
							/>
							<div className="flex flex-col min-w-0 flex-1">
								<span className="truncate text-[12px]">
									{cp.message || "(no message)"}
								</span>
								<span className="text-fg-dim text-[10.5px] font-mono truncate">
									{cp.time}
								</span>
							</div>
						</div>
					);
				})}
			</div>
			{menu && (
				<div
					className="fixed z-100 min-w-[160px] bg-bg-elevated border border-border-subtle rounded-md shadow-2xl py-1 text-[12px]"
					style={{ left: menu.x, top: menu.y }}
					onMouseDown={(e) => e.stopPropagation()}
					onContextMenu={(e) => e.preventDefault()}
				>
					<CheckpointMenuItem
						icon={<ClipboardCopy size={12} />}
						label="Copy hash"
						onClick={() => handleCopyHash(menu.checkpoint)}
					/>
					<div className="my-1 border-t border-border-subtle" />
					<CheckpointMenuItem
						icon={<RotateCcw size={12} />}
						label="Restore"
						onClick={() => handleRestore(menu.checkpoint)}
						danger
					/>
				</div>
			)}
		</div>
	);
}

function CheckpointMenuItem({
	icon,
	label,
	onClick,
	danger,
}: {
	icon: React.ReactNode;
	label: string;
	onClick: () => void;
	danger?: boolean;
}) {
	return (
		<button
			type="button"
			className={`w-full flex items-center gap-2 px-3 py-1.5 text-left hover:bg-bg-hover transition-colors ${danger ? "text-red-400 hover:text-red-300" : "text-fg-muted hover:text-fg"}`}
			onClick={onClick}
		>
			<span className="w-3.5 flex items-center justify-center shrink-0">
				{icon}
			</span>
			<span>{label}</span>
		</button>
	);
}
