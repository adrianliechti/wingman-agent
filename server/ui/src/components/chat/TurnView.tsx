import { ChevronRight } from "lucide-react";
import { memo, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ChatEntry } from "../../types/session";
import { sampleSpinnerVerb } from "../../spinnerVerbs";
import type { Phase } from "../../types/protocol";
import { EntryView, ToolGroupView } from "./entries";
import { summarizeSteps } from "./stepSummary";
import type { Turn } from "./turns";
import { TurnReviewCard } from "./TurnReviews";

interface TurnViewProps {
	turn: Turn;
	isActive: boolean;
	phase: Phase;
	applyPendingAnchor: () => void;
	onOpenFile?: (path: string, line?: number) => void;
}

export const TurnView = memo(function TurnView({
	turn,
	isActive,
	phase,
	applyPendingAnchor,
	onOpenFile,
}: TurnViewProps) {
	// Only tool runs fold into a summary; thoughts fold themselves and progress
	// text stays readable.
	const canCollapse =
		!isActive &&
		turn.reply.length > 0 &&
		turn.working.some((entry) => entry.type === "tool");
	const last = turn.working.at(-1);
	const thinkingVisibly =
		phase === "thinking" && last?.type === "reasoning" && !!last.content;
	const [override, setOverride] = useState<boolean | null>(null);
	const expanded = override ?? !canCollapse;
	// Manual toggle keeps scrollTop unchanged: the disclosure header sits above
	// the content that grows/shrinks, so it stays visually fixed and the steps
	// reveal/hide below it — no scroll jump. (applyPendingAnchor still runs in
	// the effect to preserve position on auto-collapse when a turn finishes.)
	const setExpanded = (value: boolean) => {
		setOverride(value);
	};

	const prevExpandedRef = useRef(expanded);
	useLayoutEffect(() => {
		if (prevExpandedRef.current === expanded) return;
		prevExpandedRef.current = expanded;
		applyPendingAnchor();
	}, [expanded, applyPendingAnchor]);

	return (
		<>
			{turn.user && <EntryView entry={turn.user} isStreaming={false} />}
			{turn.working.length > 0 &&
				(expanded ? (
					<>
						<WorkingExpanded
							entries={turn.working}
							isActive={isActive}
							phase={phase}
							canCollapse={canCollapse}
							onCollapse={() => setExpanded(false)}
							onOpenFile={onOpenFile}
						/>
						{isActive &&
							!(phase === "streaming" && turn.reply.length > 0) &&
							!thinkingVisibly && <PhaseIndicator />}
					</>
				) : (
					<WorkingHeader
						entries={turn.working}
						expanded={false}
						onToggle={() => setExpanded(true)}
						className="mb-4"
					/>
				))}
			{turn.reply.map((entry, index) => (
				<EntryView
					key={entry.id}
					entry={entry}
					isStreaming={
						isActive && phase === "streaming" && index === turn.reply.length - 1
					}
				/>
			))}
			{isActive && turn.working.length === 0 && turn.reply.length === 0 && (
				<PhaseIndicator />
			)}
			<TurnReviewCard inputId={turn.user?.inputId} onOpenFile={onOpenFile} />
		</>
	);
}, areTurnPropsEqual);

function areTurnPropsEqual(prev: TurnViewProps, next: TurnViewProps) {
	if (!sameTurnEntries(prev.turn, next.turn)) return false;
	if (prev.isActive !== next.isActive) return false;
	if (prev.onOpenFile !== next.onOpenFile) return false;
	if (!prev.isActive && !next.isActive) return true;
	return (
		prev.phase === next.phase &&
		prev.turn.working.length === next.turn.working.length
	);
}

function sameTurnEntries(a: Turn, b: Turn) {
	if (a.key !== b.key || a.user !== b.user) return false;
	if (a.reply.length !== b.reply.length) return false;
	for (let i = 0; i < a.reply.length; i++) {
		if (a.reply[i] !== b.reply[i]) return false;
	}
	if (a.working.length !== b.working.length) return false;
	for (let i = 0; i < a.working.length; i++) {
		if (a.working[i] !== b.working[i]) return false;
	}
	return true;
}

const WorkingExpanded = memo(function WorkingExpanded({
	entries,
	isActive,
	phase,
	canCollapse,
	onCollapse,
	onOpenFile,
}: {
	entries: ChatEntry[];
	isActive: boolean;
	phase: Phase;
	canCollapse: boolean;
	onCollapse: () => void;
	onOpenFile?: (path: string, line?: number) => void;
}) {
	const nodes: React.ReactNode[] = [];
	let i = 0;
	while (i < entries.length) {
		const entry = entries[i];
		if (entry.type === "tool") {
			const start = i;
			while (i < entries.length && entries[i].type === "tool") i++;
			const slice = entries.slice(start, i);
			const isTrailing = isActive && i === entries.length;
			nodes.push(
				<ToolGroupView
					key={slice[0].id}
					entries={slice}
					isTrailing={isTrailing}
					phase={phase}
					onOpenFile={onOpenFile}
				/>,
			);
			continue;
		}
		const isLastWorking = i === entries.length - 1;
		const isStreaming =
			isActive &&
			isLastWorking &&
			phase === "thinking" &&
			entry.type === "reasoning";
		nodes.push(
			<EntryView
				key={entry.id}
				entry={entry}
				isStreaming={isStreaming}
				live={isActive}
			/>,
		);
		i++;
	}

	return (
		<>
			{canCollapse && (
				<WorkingHeader
					entries={entries}
					expanded
					onToggle={onCollapse}
					className="mb-1"
				/>
			)}
			{nodes}
		</>
	);
});

// Disclosure header for the working steps; the same row toggles both ways.
const WorkingHeader = memo(function WorkingHeader({
	entries,
	expanded,
	onToggle,
	className = "",
}: {
	entries: ChatEntry[];
	expanded: boolean;
	onToggle: () => void;
	className?: string;
}) {
	return (
		<button
			type="button"
			onClick={onToggle}
			aria-expanded={expanded}
			className={`flex cursor-pointer items-center gap-2 py-[3px] text-[12px] text-fg-dim transition-colors hover:text-fg-muted ${className}`}
		>
			<ChevronRight
				size={12}
				className={`shrink-0 transition-transform ${expanded ? "rotate-90" : ""}`}
			/>
			<span>{summarizeSteps(entries)}</span>
		</button>
	);
});

function PhaseIndicator() {
	const [verb] = useState(sampleSpinnerVerb);

	const [clock, setClock] = useState(() => {
		const startedAt = Date.now();
		return { startedAt, now: startedAt };
	});
	useEffect(() => {
		const id = setInterval(() => {
			setClock((prev) => ({ ...prev, now: Date.now() }));
		}, 1000);
		return () => clearInterval(id);
	}, []);

	const elapsed = Math.max(0, Math.floor((clock.now - clock.startedAt) / 1000));

	return (
		<div className="mb-4 flex items-baseline gap-2 py-[3px] text-[12px]">
			<span className="text-shimmer">{verb}…</span>
			<span className="text-fg-dim/70 tabular-nums">
				{elapsed}s · esc to interrupt
			</span>
		</div>
	);
}
