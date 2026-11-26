import type { ReactNode } from "react";

export function Card({
	title,
	detail,
	className,
	children,
}: {
	title: string;
	detail?: string;
	className?: string;
	children: ReactNode;
}) {
	return (
		<section
			className={`flex min-h-0 flex-col overflow-hidden rounded-md border border-border-subtle bg-bg-surface/10 ${className ?? ""}`}
		>
			<div className="flex shrink-0 items-baseline gap-2 border-b border-border-subtle px-2.5 py-1.5">
				<span className="text-[10px] font-medium uppercase tracking-wider text-fg-dim">
					{title}
				</span>
				{detail && (
					<span className="min-w-0 truncate text-[9px] text-fg-dim">
						{detail}
					</span>
				)}
			</div>
			<div className="min-h-0 overflow-y-auto">{children}</div>
		</section>
	);
}
