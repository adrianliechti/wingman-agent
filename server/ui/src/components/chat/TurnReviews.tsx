import { lazy, Suspense, useContext, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
	ChevronRight,
	CircleAlert,
	CircleCheck,
	CircleDashed,
	CircleX,
	FileDiff,
} from "lucide-react";
import {
	getTurnReview,
	turnReviewKey,
	undoTurn,
	type TurnFileChange,
	type TurnFileStats,
} from "../../api/turnReviews";
import { statusColor } from "../../utils/diffStatus";
import { useToast } from "../ui/Feedback";
import { TurnReviewsContext } from "./turnReviewContext";
const ReviewDiff = lazy(async () => {
	await import("../../monacoRuntime");
	return { default: (await import("./TurnReviewDiff")).TurnReviewDiff };
});
const validations: Record<string, { label: string; icon: React.ReactNode }> = {
	passed: {
		label: "Validation passed",
		icon: <CircleCheck size={12} className="text-success" />,
	},
	failed: {
		label: "Validation failed",
		icon: <CircleX size={12} className="text-danger" />,
	},
	outdated: {
		label: "Checks predate the latest edits",
		icon: <CircleAlert size={12} />,
	},
	not_confirmed: {
		label: "Validation not confirmed",
		icon: <CircleDashed size={12} />,
	},
	not_run: { label: "Validation not run", icon: <CircleDashed size={12} /> },
};
const fileStatus = {
	A: "Added",
	D: "Deleted",
	M: "Modified",
};

export function TurnReviewCard({
	inputId,
	onOpenFile,
}: {
	inputId?: string;
	onOpenFile?: (path: string) => void;
}) {
	const context = useContext(TurnReviewsContext);
	const review = context.reviews.get(inputId ?? "");
	const [expanded, setExpanded] = useState(false);
	const [undoing, setUndoing] = useState(false);
	const client = useQueryClient();
	const toast = useToast();
	const query = useQuery({
		queryKey: [...turnReviewKey(context.session), review?.id],
		enabled: !!review && expanded,
		queryFn: ({ signal }) => getTurnReview(context.session, review!.id, signal),
	});
	if (
		!review ||
		(!review.files.length &&
			!review.checks.length &&
			review.outcome !== "incomplete")
	)
		return null;
	const count = review.files.length;
	const added = review.files.reduce((sum, file) => sum + file.added, 0);
	const removed = review.files.reduce((sum, file) => sum + file.removed, 0);
	const validation =
		validations[review.validation] ?? validations.not_confirmed;
	const detail = query.data;
	return (
		<div data-turn-review className="mb-6 text-[12px]">
			<div className="flex items-center gap-2 text-fg-dim">
				<span className="h-px flex-1 bg-border-subtle" />
				<button
					type="button"
					className="group flex min-w-0 items-center gap-1.5 rounded px-1.5 py-0.5 transition-colors hover:bg-bg-hover hover:text-fg-muted"
					onClick={() => setExpanded(!expanded)}
					aria-expanded={expanded}
				>
					<FileDiff size={12} className="shrink-0" />
					<span className="truncate">
						{count} {count === 1 ? "file" : "files"} changed
					</span>
					{(added > 0 || removed > 0) && (
						<LineCounts added={added} removed={removed} />
					)}
					<span aria-hidden="true">·</span>
					<span className="shrink-0">{validation.icon}</span>
					<span className="truncate">
						{validation.label}
						{review.undone ? " · Undone" : ""}
						{review.outcome === "incomplete" ? " · Incomplete" : ""}
					</span>
					<span className="sr-only">
						{expanded ? "Hide changes" : "Review changes"}
					</span>
					<ChevronRight
						size={12}
						className={`shrink-0 transition-transform ${expanded ? "rotate-90" : ""}`}
					/>
				</button>
				<span className="h-px flex-1 bg-border-subtle" />
			</div>
			{expanded && (
				<div className="mt-2 rounded-lg border border-border-subtle bg-bg-surface/50">
					<p className="px-3 pt-2.5 pb-1.5 text-fg-dim">
						Turn {review.outcome}.{" "}
						{review.untracked
							? "These diffs cover tracked edits; commands and external tools may have made other changes."
							: "These diffs start from the files as the agent found them."}
					</p>
					{query.error && (
						<p role="alert" className="px-3 pb-2 text-danger">
							{String(query.error)}
						</p>
					)}
					{count > 0 && (
						<div className="px-1.5 pb-1">
							{review.files.map((file, index) => (
								<ReviewFile
									key={file.path}
									file={file}
									change={detail?.files[index]}
									onOpenFile={onOpenFile}
								/>
							))}
						</div>
					)}
					{review.checks.length > 0 && (
						<div className="border-t border-border-subtle px-3 py-2">
							{review.checks.map((check, index) => (
								<div
									key={index}
									className="mb-1 font-mono break-words last:mb-0"
								>
									<span
										className={
											check.outcome === "passed"
												? "text-success"
												: "text-warning"
										}
									>
										{check.outcome.replaceAll("_", " ")}
										{check.exitCode !== undefined
											? ` (exit ${check.exitCode})`
											: ""}
									</span>{" "}
									· {check.command}
									{check.workdir && (
										<span className="text-fg-dim"> · {check.workdir}</span>
									)}
								</div>
							))}
						</div>
					)}
					{count > 0 && (
						<div className="flex items-center gap-3 border-t border-border-subtle px-3 py-2">
							<button
								type="button"
								className="rounded border border-border px-2.5 py-1 text-fg-muted transition-colors hover:bg-bg-hover hover:text-fg disabled:opacity-40"
								disabled={
									!detail ||
									!context.available ||
									context.active ||
									review.uncertain ||
									review.undone ||
									undoing
								}
								onClick={async () => {
									setUndoing(true);
									try {
										await undoTurn(context.session, review.id);
										await client.invalidateQueries({
											queryKey: turnReviewKey(context.session),
										});
										toast({ title: "Tracked edits undone", tone: "success" });
									} catch (error) {
										toast({
											title: "Could not undo this turn",
											description: String(error),
											tone: "error",
										});
									}
									setUndoing(false);
								}}
							>
								{review.undone
									? "Edits undone"
									: undoing
										? "Undoing…"
										: "Undo tracked edits"}
							</button>
							<p className="text-fg-dim">
								Undo stops if a file has changed since the agent edited it.
							</p>
						</div>
					)}
				</div>
			)}
		</div>
	);
}

function LineCounts({ added, removed }: { added: number; removed: number }) {
	return (
		<span className="shrink-0 font-mono text-[11px] tabular-nums">
			<span className="text-success">+{added}</span>{" "}
			<span className="text-danger">−{removed}</span>
		</span>
	);
}

function ReviewFile({
	file,
	change,
	onOpenFile,
}: {
	file: TurnFileStats;
	change?: TurnFileChange;
	onOpenFile?: (path: string) => void;
}) {
	const [open, setOpen] = useState(false);
	const letter = !file.beforeExists ? "A" : !file.afterExists ? "D" : "M";
	const slash = file.path.lastIndexOf("/");
	return (
		<details onToggle={(event) => setOpen(event.currentTarget.open)}>
			<summary className="flex cursor-pointer list-none items-center gap-2 rounded px-1.5 py-1 hover:bg-bg-hover [&::-webkit-details-marker]:hidden">
				<ChevronRight
					size={12}
					className={`shrink-0 text-fg-dim transition-transform ${open ? "rotate-90" : ""}`}
				/>
				<span
					className={`w-3 shrink-0 text-center font-mono text-[11px] font-bold ${statusColor(letter)}`}
					title={fileStatus[letter]}
				>
					{letter}
				</span>
				<span
					className="min-w-0 flex-1 truncate font-mono text-[11px]"
					title={file.path}
				>
					<span className="text-fg">{file.path.slice(slash + 1)}</span>
					{slash > 0 && (
						<span className="ml-2 text-fg-dim">
							{file.path.slice(0, slash)}
						</span>
					)}
				</span>
				<LineCounts added={file.added} removed={file.removed} />
			</summary>
			{open && (
				<div className="mt-1 mb-2 ml-6">
					{onOpenFile && file.afterExists && (
						<button
							type="button"
							className="mb-1 text-fg-dim underline-offset-2 hover:text-fg hover:underline"
							onClick={() => onOpenFile(file.path)}
						>
							Open file
						</button>
					)}
					<div className="h-80 overflow-hidden rounded border border-border-subtle">
						{change ? (
							<Suspense
								fallback={<p className="p-2 text-fg-dim">Loading diff…</p>}
							>
								<ReviewDiff file={change} />
							</Suspense>
						) : (
							<p role="status" className="p-2 text-fg-dim">
								Loading diff…
							</p>
						)}
					</div>
				</div>
			)}
		</details>
	);
}
