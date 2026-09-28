import { Check, Compass, Wrench, Zap } from "lucide-react";
import { useId, useState } from "react";
import type { ModeOption } from "../api/sessions";
import { FloatingMenu } from "./ui/Floating";

export type { ModeOption } from "../api/sessions";

function isPlanLike(id: string): boolean {
	return /plan|read|only/i.test(id);
}

function modeColor(id: string): string {
	if (id.toLowerCase() === "unattended") return "text-danger";
	return "";
}

interface Props {
	modes: ModeOption[];
	current: string;
	onSelect: (id: string) => void;
	unattended?: boolean;
	onUnattendedChange?: (enabled: boolean) => void;
}

export function ModePicker({
	modes,
	current,
	onSelect,
	unattended,
	onUnattendedChange,
}: Props) {
	const [open, setOpen] = useState(false);
	const [button, setButton] = useState<HTMLButtonElement | null>(null);
	const id = useId();

	if (modes.length === 0) return null;

	const active = modes.find((m) => m.id === current);
	const label = active?.name ?? current ?? "Mode";
	const title = `Mode: ${label}${unattended ? " · Unattended on" : ""}`;
	const ActiveIcon = isPlanLike(current) ? Compass : Wrench;

	return (
		<div data-composer-mode className="relative shrink-0 max-w-[120px]">
			<button
				ref={setButton}
				type="button"
				onClick={() => setOpen((v) => !v)}
				className={`picker-control flex items-center gap-1.5 px-2 h-7 max-w-full rounded text-[11.5px] cursor-pointer transition-colors hover:bg-bg-hover ${
					unattended
						? "text-danger"
						: open
							? "bg-bg-hover text-fg"
							: "text-fg-muted hover:text-fg"
				} ${open ? "bg-bg-hover" : ""}`}
				title={title}
				aria-label={title}
				aria-haspopup="menu"
				aria-expanded={open}
			>
				<ActiveIcon
					size={12}
					className={`shrink-0 ${unattended ? "" : modeColor(current) || "opacity-70"}`}
					aria-hidden="true"
				/>
				<span className="truncate">{label}</span>
			</button>
			<FloatingMenu
				open={open}
				onOpenChange={setOpen}
				reference={button}
				placement="top-start"
				label="Mode and unattended operation"
				className="z-[100] w-[248px] bg-bg-elevated/95 backdrop-blur-sm border border-border rounded-md shadow-xl p-1"
			>
				<div role="group" aria-label="Mode">
					{modes.map((opt) => {
						const isActive = opt.id === current;
						const OptIcon = isPlanLike(opt.id) ? Compass : Wrench;
						return (
							<button
								type="button"
								role="menuitemradio"
								aria-checked={isActive}
								aria-label={opt.name}
								aria-describedby={
									opt.description ? `${id}-${opt.id}` : undefined
								}
								key={opt.id}
								onClick={() => {
									if (!isActive) onSelect(opt.id);
									setOpen(false);
								}}
								className={`picker-control w-full flex items-center gap-2 rounded px-2 py-1.5 text-left cursor-pointer transition-colors focus-visible:bg-bg-hover ${
									isActive
										? "text-fg"
										: "text-fg-muted hover:bg-bg-hover hover:text-fg"
								}`}
							>
								<OptIcon
									size={13}
									className={`shrink-0 ${modeColor(opt.id) || "text-fg-dim"}`}
									aria-hidden="true"
								/>
								<div className="min-w-0 flex-1">
									<div className="text-[12px] leading-tight">{opt.name}</div>
									{opt.description && (
										<div
											id={`${id}-${opt.id}`}
											className="text-[10.5px] text-fg-dim leading-snug truncate"
											title={opt.description}
										>
											{opt.description}
										</div>
									)}
								</div>
								<Check
									size={12}
									className={`shrink-0 ${isActive ? "opacity-100" : "opacity-0"}`}
									aria-hidden="true"
								/>
							</button>
						);
					})}
				</div>
				{unattended !== undefined && onUnattendedChange && (
					<>
						<div
							role="separator"
							className="border-t border-border my-1 mx-1"
						/>
						<button
							type="button"
							role="menuitemcheckbox"
							aria-checked={unattended}
							aria-label="Unattended"
							aria-describedby={`${id}-unattended`}
							onClick={() => onUnattendedChange(!unattended)}
							className="picker-control flex w-full items-center gap-2 rounded px-2 py-1.5 text-left cursor-pointer transition-colors hover:bg-bg-hover focus-visible:bg-bg-hover"
						>
							<Zap
								size={13}
								className={`shrink-0 ${unattended ? "text-danger" : "text-fg-dim"}`}
								aria-hidden="true"
							/>
							<div className="min-w-0 flex-1">
								<div
									className={`text-[12px] leading-tight ${unattended ? "text-danger" : "text-fg-muted"}`}
								>
									Unattended
								</div>
								<div
									id={`${id}-unattended`}
									className="text-[10.5px] text-fg-dim leading-snug"
								>
									Auto-approve, no questions.
								</div>
							</div>
							<span
								className={`flex h-3.5 w-6 shrink-0 items-center rounded-full p-0.5 transition-colors ${unattended ? "bg-danger" : "bg-fg-dim/30"}`}
								aria-hidden="true"
							>
								<span
									className={`size-2.5 rounded-full bg-bg-elevated shadow-sm transition-transform ${unattended ? "translate-x-2.5" : "translate-x-0"}`}
								/>
							</span>
						</button>
					</>
				)}
			</FloatingMenu>
		</div>
	);
}
