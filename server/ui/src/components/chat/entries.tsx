import { VirtualOutput } from "./VirtualOutput";
import {
	Brain,
	ChevronRight,
	FilePen,
	FileSymlink,
	FileText,
	Globe,
	LoaderCircle,
	Search,
	SquareTerminal,
	Trash2,
	Wrench,
} from "lucide-react";
import { memo, useEffect, useRef, useState } from "react";
import type { ChatEntry } from "../../types/session";
import type { Phase, ToolLocation } from "../../types/protocol";
import { MarkdownContent } from "../MarkdownContent";
import { looksLikeDiffPath, shouldRenderDiff } from "./diffOutput";
import { type ToolBucket, toolBucket } from "./stepSummary";

export const EntryView = memo(function EntryView({
	entry,
	isStreaming,
	live = false,
}: {
	entry: ChatEntry;
	isStreaming: boolean;
	live?: boolean;
}) {
	if (entry.type === "reasoning") {
		return (
			<ReasoningView entry={entry} isStreaming={isStreaming} live={live} />
		);
	}
	if (entry.type === "notice") {
		return (
			<div
				data-entry-id={entry.id}
				role="note"
				className="mb-4 pl-3.5 text-[11px] leading-relaxed whitespace-pre-wrap break-words text-fg-dim font-mono select-text"
			>
				{entry.content}
			</div>
		);
	}

	if (entry.type === "user") {
		return (
			<div
				data-entry-id={entry.id}
				className="mb-5 flex justify-end select-text"
			>
				<div className="min-w-0 max-w-[85%] rounded-lg border border-border-subtle bg-bg-surface px-3 py-2 text-[13px] leading-relaxed break-words text-fg">
					{entry.content && (
						<div className="whitespace-pre-wrap">{entry.content}</div>
					)}
					{entry.files && entry.files.length > 0 && (
						<div
							className={`flex flex-wrap gap-1 ${entry.content ? "mt-2" : ""}`}
						>
							{entry.files.map((path) => (
								<span
									key={path}
									className="max-w-[240px] truncate rounded bg-bg-active px-1.5 py-0.5 font-mono text-[11px] text-fg-muted"
									title={path}
								>
									{path.split("/").pop() || path}
								</span>
							))}
						</div>
					)}
					<EntryImages
						entry={entry}
						spaced={!!entry.content || (entry.files?.length ?? 0) > 0}
					/>
				</div>
			</div>
		);
	}

	return (
		<div
			data-entry-id={entry.id}
			className="mb-4 min-w-0 select-text text-[13px] leading-[1.65] break-words"
		>
			<MarkdownContent text={entry.content} streaming={isStreaming} />
			<EntryImages entry={entry} spaced={!!entry.content} />
		</div>
	);
});

function EntryImages({ entry, spaced }: { entry: ChatEntry; spaced: boolean }) {
	if (!entry.images?.length) return null;
	return (
		<div className={`flex flex-wrap gap-1.5 ${spaced ? "mt-2" : ""}`}>
			{entry.images.map((src, i) => (
				<a
					key={`${entry.id}-img-${i}`}
					href={src}
					target="_blank"
					rel="noreferrer"
					className="block overflow-hidden rounded bg-bg-active"
				>
					<img
						src={src}
						alt="attachment"
						className="block max-h-40 max-w-[240px] object-contain"
					/>
				</a>
			))}
		</div>
	);
}

const ReasoningView = memo(function ReasoningView({
	entry,
	isStreaming,
	live,
}: {
	entry: ChatEntry;
	isStreaming: boolean;
	live: boolean;
}) {
	// Thoughts stay open while their turn runs and fold when it settles, where
	// the transcript keeps the reader's scroll position.
	const [open, setOpen] = useState<boolean | null>(null);
	const expanded = open ?? live;
	const summary = entry.content || "";
	if (!summary) return null;

	return (
		<div data-entry-id={entry.id} className="mb-3 select-text">
			<button
				type="button"
				onClick={() => setOpen(!expanded)}
				aria-expanded={expanded}
				className="group flex items-center gap-2 py-[3px] text-[12px] text-fg-dim transition-colors hover:text-fg-muted"
			>
				<DisclosureIcon
					icon={<Brain size={12} aria-hidden="true" />}
					expanded={expanded}
				/>
				<span className={isStreaming ? "text-shimmer" : ""}>
					{isStreaming
						? "Thinking"
						: entry.reasoningDuration !== undefined
							? `Thought for ${formatReasoningDuration(entry.reasoningDuration)}`
							: "Thought"}
				</span>
			</button>
			{expanded && (
				<div className="mt-1 mb-1 ml-[5px] border-l border-border-subtle pl-[15px] text-[12px] leading-relaxed whitespace-pre-wrap break-words text-fg-dim">
					{summary}
					{isStreaming && (
						<span className="ml-0.5 inline-block h-[10px] w-[3px] animate-[pulse_1.4s_ease-in-out_infinite] bg-fg-dim/40 align-text-bottom" />
					)}
				</div>
			)}
		</div>
	);
});

function formatReasoningDuration(ms: number): string {
	if (ms < 1000) return "<1s";
	const seconds = Math.round(ms / 1000);
	return seconds < 60
		? `${seconds}s`
		: `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

export const ToolGroupView = memo(function ToolGroupView({
	entries,
	isTrailing,
	phase,
	onOpenFile,
}: {
	entries: ChatEntry[];
	isTrailing: boolean;
	phase: Phase;
	onOpenFile?: (path: string, line?: number) => void;
}) {
	return (
		<div className="mb-3">
			{entries.map((entry) => {
				const running =
					isTrailing && phase !== "idle" && entry.toolResult === undefined;
				return (
					<ToolRow
						key={entry.id}
						entry={entry}
						running={running}
						onOpenFile={onOpenFile}
					/>
				);
			})}
		</div>
	);
});

function parseArgs(json?: string): Record<string, unknown> | null {
	if (!json) return null;
	try {
		const v: unknown = JSON.parse(json);
		if (v && typeof v === "object" && !Array.isArray(v)) {
			return v as Record<string, unknown>;
		}
	} catch {}
	return null;
}

const ToolRow = memo(function ToolRow({
	entry,
	running,
	onOpenFile,
}: {
	entry: ChatEntry;
	running: boolean;
	onOpenFile?: (path: string, line?: number) => void;
}) {
	const [expanded, setExpanded] = useState(false);
	const [showAll, setShowAll] = useState(false);
	const [hovered, setHovered] = useState(false);
	const displayHint = entry.toolHint ? truncate(entry.toolHint, 80) : "";
	const result = (entry.toolResult || "").replace(/\n+$/, "");
	const isDiff = shouldRenderDiff(entry.toolKind, entry.toolName, result);
	const args = isDiff ? null : parseArgs(entry.toolArgs);
	const isCommandTool =
		entry.toolName === "exec" ||
		entry.toolName === "shell" ||
		entry.toolName === "exec_command" ||
		entry.toolName === "Run command" ||
		(entry.toolKind === "execute" && typeof args?.command === "string");
	const toolLabel =
		entry.toolName === "exec" ||
		entry.toolName === "shell" ||
		entry.toolName === "exec_command"
			? "Run command"
			: entry.toolName;
	const headerHint = displayHint
		? `${isCommandTool ? "$ " : ""}${displayHint}`
		: "";
	const locations = entry.toolLocations ?? [];
	const limit = isDiff ? 4000 : 2000;
	const overflows = result.length > limit;
	const shown = showAll ? result : truncate(result, limit);

	const command =
		isCommandTool && typeof args?.command === "string" ? args.command : "";
	const workdir =
		isCommandTool && typeof args?.workdir === "string" ? args.workdir : "";
	const argEntries =
		!command && args
			? Object.entries(args).map(([key, value]) => ({
					key,
					value: truncate(
						typeof value === "string" ? value : JSON.stringify(value),
						600,
					),
				}))
			: [];
	const hasInputDetails = !!command || argEntries.length > 0;
	const hasDetails = hasInputDetails || !!result;
	const canExpand = hasDetails && !entry.toolPartial;

	return (
		<div
			data-entry-id={entry.id}
			onMouseEnter={() => setHovered(true)}
			onMouseLeave={() => setHovered(false)}
		>
			<div className="flex min-w-0 flex-nowrap items-center gap-1 overflow-hidden py-[3px]">
				<button
					type="button"
					aria-expanded={canExpand ? expanded : undefined}
					aria-disabled={!canExpand}
					tabIndex={canExpand ? 0 : -1}
					className={`group flex min-w-0 items-center gap-2 overflow-hidden text-left text-[12px] text-fg-muted transition-colors ${
						canExpand ? "cursor-pointer hover:text-fg" : "cursor-default"
					}`}
					onClick={() => canExpand && setExpanded(!expanded)}
				>
					{running ? (
						<span className="flex w-3 shrink-0 items-center justify-center text-fg-dim">
							<LoaderCircle
								size={12}
								className="animate-spin"
								aria-label={
									entry.toolPartial ? "Preparing tool" : "Tool running"
								}
							/>
						</span>
					) : (
						<DisclosureIcon
							icon={<ToolIcon entry={entry} />}
							expanded={expanded}
							disabled={!canExpand}
						/>
					)}
					<span className="shrink-0">{toolLabel}</span>
					{headerHint && (
						<span
							className="min-w-0 flex-1 truncate font-mono text-[11px] text-fg-dim"
							title={entry.toolHint}
						>
							{headerHint}
						</span>
					)}
				</button>
				{onOpenFile && locations.length > 0 && (
					<div className="flex min-w-0 max-w-[45%] shrink-0 items-center gap-1 overflow-hidden">
						{locations.map((location, index) => (
							<button
								key={`${location.path}:${location.line ?? ""}:${index}`}
								type="button"
								className="min-w-0 max-w-[260px] cursor-pointer truncate rounded px-1 py-px font-mono text-[11px] text-fg-dim transition-colors hover:bg-bg-hover hover:text-fg"
								title={location.path}
								onClick={() => onOpenFile(location.path, location.line)}
							>
								{toolLocationLabel(location)}
							</button>
						))}
					</div>
				)}
			</div>
			{expanded && canExpand && (
				<div
					className={`mt-1 mb-1.5 ml-5 rounded-md bg-bg-surface px-3 py-2 font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all text-fg-dim ${
						showAll ? "max-h-[50vh] overflow-y-auto overscroll-contain" : ""
					}`}
				>
					{command ? (
						<div className="text-fg">
							<span className="text-fg-dim">$ </span>
							{command}
							{workdir && <span className="text-fg-dim"> [in {workdir}]</span>}
						</div>
					) : (
						argEntries.map(({ key, value }) => (
							<div key={key}>
								<span className="text-fg-muted">{key}:</span> {value}
							</div>
						))
					)}
					{hasInputDetails && result && (
						<div className="my-1.5 h-px bg-border-subtle" />
					)}
					{result &&
						(showAll && result.length > 12000 ? (
							<VirtualOutput
								text={result}
								lineClass={isDiff ? diffLineClass : undefined}
							/>
						) : isDiff ? (
							<DiffText text={shown} />
						) : (
							shown
						))}
				</div>
			)}
			{expanded && result && (
				<div className="mb-1 flex h-4 items-center gap-3 pl-5">
					{overflows && (
						<button
							type="button"
							className="text-[10px] text-fg-dim hover:text-fg transition-colors cursor-pointer"
							onClick={() => setShowAll((s) => !s)}
						>
							{showAll
								? "Show less"
								: `Show all (${formatChars(result.length)})`}
						</button>
					)}
					{hovered && (
						<CopyTextButton text={entry.toolResult ?? ""} label="Copy" />
					)}
				</div>
			)}
		</div>
	);
});

function DiffText({ text }: { text: string }) {
	return (
		<div className="-mx-3">
			{text.split("\n").map((line, i) => (
				<div
					key={`${i}-${line.slice(0, 24)}`}
					className={`px-3 ${diffLineClass(line)}`}
				>
					{line || " "}
				</div>
			))}
		</div>
	);
}

function diffLineClass(line: string): string {
	if (
		line.startsWith("diff ") ||
		line.startsWith("index ") ||
		line.startsWith("---") ||
		line.startsWith("+++")
	) {
		return "text-fg-muted";
	}
	if (line.startsWith("@@")) {
		return "text-purple bg-[rgba(197,134,192,0.08)]";
	}
	if (line.startsWith("*** ")) {
		return "text-purple bg-[rgba(197,134,192,0.08)]";
	}
	if (line.startsWith("+")) {
		return "text-success bg-[rgba(78,201,176,0.08)]";
	}
	if (line.startsWith("-")) {
		return "text-danger bg-[rgba(244,135,113,0.08)]";
	}
	if (looksLikeDiffPath(line)) {
		return "border-y border-border-subtle bg-bg-active/50 text-fg font-medium";
	}
	if (line.startsWith("\\ No newline")) {
		return "text-fg-dim italic";
	}
	return "";
}

// Rows show their kind icon at rest and the disclosure chevron on hover or while open.
function DisclosureIcon({
	icon,
	expanded,
	disabled = false,
}: {
	icon: React.ReactNode;
	expanded: boolean;
	disabled?: boolean;
}) {
	return (
		<span className="flex w-3 shrink-0 items-center justify-center text-fg-dim">
			<span
				className={
					expanded
						? "hidden"
						: disabled
							? "flex"
							: "flex group-hover:hidden group-focus-visible:hidden"
				}
			>
				{icon}
			</span>
			{!disabled && (
				<ChevronRight
					size={12}
					aria-hidden="true"
					className={
						expanded
							? "rotate-90"
							: "hidden group-hover:block group-focus-visible:block"
					}
				/>
			)}
		</span>
	);
}

const toolIcons: Record<ToolBucket, typeof FileText> = {
	edit: FilePen,
	delete: Trash2,
	move: FileSymlink,
	execute: SquareTerminal,
	read: FileText,
	search: Search,
	fetch: Globe,
};

function ToolIcon({ entry }: { entry: ChatEntry }) {
	const bucket = toolBucket(entry);
	const Icon = bucket ? toolIcons[bucket] : Wrench;
	return <Icon size={12} aria-hidden="true" />;
}

function toolLocationLabel(location: ToolLocation): string {
	const name = location.path.replaceAll("\\", "/").split("/").pop();
	const label = name || location.path;
	return location.line ? `${label}:${location.line}` : label;
}

function truncate(text: string, max: number): string {
	return text.length <= max ? text : `${text.substring(0, max)}...`;
}

function formatChars(n: number): string {
	if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M chars`;
	if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K chars`;
	return `${n} chars`;
}

function CopyTextButton({
	text,
	label = "Copy",
	className = "",
}: {
	text: string;
	label?: string;
	className?: string;
}) {
	const [copied, setCopied] = useState(false);
	const timer = useRef<number | null>(null);

	useEffect(
		() => () => {
			if (timer.current) window.clearTimeout(timer.current);
		},
		[],
	);

	const handleClick = (e: React.MouseEvent<HTMLButtonElement>) => {
		e.stopPropagation();
		navigator.clipboard
			.writeText(text)
			.then(() => {
				setCopied(true);
				if (timer.current) window.clearTimeout(timer.current);
				timer.current = window.setTimeout(() => setCopied(false), 1200);
			})
			.catch(() => {});
	};

	return (
		<button
			type="button"
			onClick={handleClick}
			className={`text-[10px] text-fg-dim hover:text-fg transition-colors cursor-pointer ${className}`}
		>
			{copied ? "Copied" : label}
		</button>
	);
}
