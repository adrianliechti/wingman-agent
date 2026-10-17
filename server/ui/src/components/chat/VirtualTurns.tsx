import { defaultRangeExtractor, useVirtualizer } from "@tanstack/react-virtual";
import { useEffect, useRef, type RefObject } from "react";
import type { Phase } from "../../types/protocol";
import { TurnView } from "./TurnView";
import type { Turn } from "./turns";

export function VirtualTurns({
	turns,
	phase,
	scroll,
	onOpenFile,
	applyPendingAnchor,
}: {
	turns: Turn[];
	phase: Phase;
	scroll: RefObject<HTMLDivElement | null>;
	onOpenFile?: (path: string, line?: number) => void;
	applyPendingAnchor: () => void;
}) {
	"use no memo";
	// oxlint-disable-next-line react/incompatible-library -- Virtualizer state is read directly; compiler memoization is disabled.
	const virtual = useVirtualizer({
		count: turns.length,
		getScrollElement: () => scroll.current,
		estimateSize: () => 240,
		overscan: 5,
		getItemKey: (index) => turns[index].key,
		// Keep the live turn mounted for scroll anchoring and active approvals.
		rangeExtractor: (range) =>
			[...new Set([...defaultRangeExtractor(range), turns.length - 1])].sort(
				(a, b) => a - b,
			),
	});
	const restored = useRef(false);
	useEffect(() => {
		if (!restored.current) {
			restored.current = true;
			virtual.scrollToIndex(turns.length - 1, { align: "start" });
		}
	}, [virtual, turns.length]);
	return (
		<div
			data-virtual-transcript
			style={{ height: virtual.getTotalSize(), position: "relative" }}
		>
			{virtual.getVirtualItems().map((item) => (
				<div
					key={item.key}
					data-index={item.index}
					ref={virtual.measureElement}
					style={{
						position: "absolute",
						top: 0,
						left: 0,
						width: "100%",
						transform: `translateY(${item.start}px)`,
					}}
				>
					<TurnView
						turn={turns[item.index]}
						phase={phase}
						isActive={item.index === turns.length - 1 && phase !== "idle"}
						applyPendingAnchor={applyPendingAnchor}
						onOpenFile={onOpenFile}
					/>
				</div>
			))}
		</div>
	);
}
