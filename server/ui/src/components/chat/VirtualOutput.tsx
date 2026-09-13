import { useVirtualizer } from "@tanstack/react-virtual";
import { useMemo, useRef } from "react";

export function VirtualOutput({
	text,
	lineClass,
}: {
	text: string;
	lineClass?: (line: string) => string;
}) {
	"use no memo";
	const scroll = useRef<HTMLDivElement>(null);
	const lines = useMemo(() => text.split("\n"), [text]);
	// oxlint-disable-next-line react/incompatible-library -- Virtualizer state is read directly; compiler memoization is disabled.
	const virtual = useVirtualizer({
		count: lines.length,
		getScrollElement: () => scroll.current,
		estimateSize: () => 18,
		overscan: 8,
	});
	return (
		<div
			data-virtual-output
			ref={scroll}
			className="max-h-[45vh] overflow-auto overscroll-contain"
			tabIndex={0}
			aria-label="Tool output"
		>
			<div style={{ height: virtual.getTotalSize(), position: "relative" }}>
				{virtual.getVirtualItems().map((row) => (
					<div
						key={row.key}
						data-index={row.index}
						ref={virtual.measureElement}
						className={lineClass?.(lines[row.index])}
						style={{
							position: "absolute",
							top: 0,
							left: 0,
							width: "100%",
							minHeight: 18,
							transform: `translateY(${row.start}px)`,
						}}
					>
						{lines[row.index] || " "}
					</div>
				))}
			</div>
		</div>
	);
}
