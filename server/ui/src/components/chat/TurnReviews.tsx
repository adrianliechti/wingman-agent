import { lazy, Suspense, useContext, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getTurnReview, turnReviewKey, undoTurn } from "../../api/turnReviews";
import { useToast } from "../ui/Feedback";
import { TurnReviewsContext } from "./turnReviewContext";
const ReviewDiff = lazy(async () => {
	await import("../../monacoRuntime");
	return { default: (await import("./TurnReviewDiff")).TurnReviewDiff };
});
const validationLabels: Record<string, string> = {
	passed: "Validation passed",
	failed: "Validation failed",
	outdated: "Checks predate the latest edits",
	not_confirmed: "Validation not confirmed",
	not_run: "Validation not run",
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
	const count = new Set(review.files.map((file) => file.path)).size;
	const detail = query.data;
	return (
		<div
			data-turn-review
			className="mb-5 rounded-lg border border-border-subtle bg-bg-surface/50 text-[12px]"
		>
			<button
				type="button"
				className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-fg-muted hover:text-fg"
				onClick={() => setExpanded(!expanded)}
				aria-expanded={expanded}
			>
				<span>
					{count} {count === 1 ? "file" : "files"} changed ·{" "}
					{validationLabels[review.validation] ?? "Validation not confirmed"}
					{review.undone ? " · Undone" : ""}
					{review.outcome === "incomplete" ? " · Incomplete" : ""}
				</span>
				<span>{expanded ? "Hide" : "Review changes"}</span>
			</button>
			{expanded && (
				<div className="border-t border-border-subtle p-3">
					<p className="mb-2 text-fg-dim">
						Turn {review.outcome}.{" "}
						{review.untracked
							? "These diffs cover tracked edits; commands and external tools may have made other changes."
							: "These diffs start from the files as the agent found them."}
					</p>
					{query.isPending && <p role="status">Loading changes…</p>}
					{query.error && (
						<p role="alert" className="text-danger">
							{String(query.error)}
						</p>
					)}
					{detail?.files.map((file, index) => (
						<ReviewFile
							key={`${file.path}:${index}`}
							file={file}
							onOpenFile={onOpenFile}
						/>
					))}
					{review.checks.map((check, index) => (
						<div key={index} className="mb-1 break-words font-mono">
							<span
								className={
									check.outcome === "passed" ? "text-success" : "text-warning"
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
					{count > 0 && (
						<button
							type="button"
							className="mt-3 rounded border border-border px-3 py-2 text-fg-muted hover:bg-bg-hover disabled:opacity-40"
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
								} finally {
									setUndoing(false);
								}
							}}
						>
							{review.undone
								? "Edits undone"
								: undoing
									? "Undoing…"
									: "Undo tracked edits"}
						</button>
					)}
					{count > 0 && (
						<p className="mt-2 text-fg-dim">
							Undo stops if a file has changed since the agent edited it.
						</p>
					)}
				</div>
			)}
		</div>
	);
}

function ReviewFile({
	file,
	onOpenFile,
}: {
	file: import("../../api/turnReviews").TurnFileChange;
	onOpenFile?: (path: string) => void;
}) {
	const [open, setOpen] = useState(false);
	return (
		<details
			onToggle={(event) => setOpen(event.currentTarget.open)}
			className="mb-2 rounded border border-border-subtle"
		>
			<summary className="cursor-pointer px-2 py-2 font-mono">
				{file.path}
			</summary>
			{open && (
				<>
					<button
						type="button"
						className="px-2 py-1 underline"
						onClick={() => onOpenFile?.(file.path)}
					>
						Open file
					</button>
					<div className="h-80">
						<Suspense fallback={<p>Loading diff…</p>}>
							<ReviewDiff file={file} />
						</Suspense>
					</div>
				</>
			)}
		</details>
	);
}
