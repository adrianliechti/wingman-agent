import {
	ClipboardCopy,
	FileText,
	RotateCcw,
} from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import type { DiffEntry, ServerMessage } from "../types/protocol";

interface Props {
	onOpenDiff?: (path: string) => void;
	onOpenFile?: (path: string) => void;
	subscribe?: (handler: (msg: ServerMessage) => void) => () => void;
}

interface MenuState {
	x: number;
	y: number;
	diff: DiffEntry;
}

export function DiffsPanel({ onOpenDiff, onOpenFile, subscribe }: Props) {
	const [diffs, setDiffs] = useState<DiffEntry[]>([]);
	const [menu, setMenu] = useState<MenuState | null>(null);

	const loadDiffs = useCallback(async () => {
		try {
			const res = await fetch("/api/diffs");
			if (!res.ok) {
				setDiffs([]);
				return;
			}
			const data: DiffEntry[] = await res.json();
			setDiffs(data);
		} catch {
			setDiffs([]);
		}
	}, []);

	useEffect(() => {
		// eslint-disable-next-line react-hooks/set-state-in-effect -- standard data-load on mount
		loadDiffs();
	}, [loadDiffs]);

	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "diffs_changed") {
				loadDiffs();
			}
		});
	}, [subscribe, loadDiffs]);

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

	const handleOpen = (diff: DiffEntry) => {
		setMenu(null);
		onOpenFile?.(diff.path);
	};

	const handleCopy = (diff: DiffEntry) => {
		setMenu(null);
		// The patch is already in memory — no fetch — so we can pass a resolved
		// blob to ClipboardItem without losing the user gesture.
		navigator.clipboard
			.write([
				new ClipboardItem({
					"text/plain": new Blob([diff.patch], { type: "text/plain" }),
				}),
			])
			.catch((e) => alert(`Copy failed: ${e instanceof Error ? e.message : String(e)}`));
	};

	const handleRevert = async (diff: DiffEntry) => {
		setMenu(null);
		const verb =
			diff.status === "added"
				? "Delete"
				: diff.status === "deleted"
					? "Restore"
					: "Revert changes to";
		if (!confirm(`${verb} "${diff.path}"?`)) return;
		const res = await fetch(
			`/api/diffs/revert?path=${encodeURIComponent(diff.path)}`,
			{ method: "POST" },
		);
		if (!res.ok) {
			alert(`Revert failed: ${await res.text()}`);
		}
	};

	const statusLabels: Record<string, string> = {
		added: "A",
		modified: "M",
		deleted: "D",
	};
	const statusColors: Record<string, string> = {
		added: "text-success",
		modified: "text-warning",
		deleted: "text-danger",
	};

	return (
		<div className="flex flex-col h-full overflow-hidden bg-bg relative">
			<div className="overflow-y-auto flex-1 py-2">
				{diffs.length === 0 && (
					<div className="px-3 py-6 text-[11px] text-fg-dim text-center">
						No changes yet
					</div>
				)}
				{diffs.map((diff) => {
					const fileName = diff.path.split("/").pop() || diff.path;
					const dir = diff.path
						.slice(0, diff.path.length - fileName.length)
						.replace(/\/$/, "");
					return (
						<div
							key={diff.path}
							className="group flex items-center gap-2 px-3 py-1 cursor-pointer text-[12px] text-fg-muted hover:bg-bg-hover hover:text-fg transition-colors"
							onClick={() => onOpenDiff?.(diff.path)}
							onContextMenu={(e) => {
								e.preventDefault();
								e.stopPropagation();
								setMenu({ x: e.clientX, y: e.clientY, diff });
							}}
							title={diff.path}
						>
							<span
								className={`text-[10.5px] font-bold w-3 text-center shrink-0 ${statusColors[diff.status]}`}
							>
								{statusLabels[diff.status]}
							</span>
							<span className="truncate font-mono text-[11.5px]">
								{fileName}
							</span>
							{dir && (
								<span className="text-fg-dim truncate font-mono text-[10.5px] ml-auto">
									{dir}
								</span>
							)}
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
					{menu.diff.status !== "deleted" && (
						<DiffMenuItem
							icon={<FileText size={12} />}
							label="Open"
							onClick={() => handleOpen(menu.diff)}
						/>
					)}
					<DiffMenuItem
						icon={<ClipboardCopy size={12} />}
						label="Copy"
						onClick={() => handleCopy(menu.diff)}
					/>
					<div className="my-1 border-t border-border-subtle" />
					<DiffMenuItem
						icon={<RotateCcw size={12} />}
						label="Revert"
						onClick={() => handleRevert(menu.diff)}
						danger
					/>
				</div>
			)}
		</div>
	);
}

function DiffMenuItem({
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
