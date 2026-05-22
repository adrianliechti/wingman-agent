import { Bot, ChevronDown } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ServerMessage } from "../types/protocol";

interface AgentInfo {
	id: string;
	name: string;
}

const BUILTIN_AGENT_ID = "wingman";

interface Props {
	// subscribe is the same WebSocket subscription helper exposed by
	// useWebSocket; the picker uses it only to react to peer-driven
	// changes (another tab switched the agent).
	subscribe?: (handler: (msg: ServerMessage) => void) => () => void;
}

// AgentPicker is the per-workspace selector for which backend new
// sessions use (wingman in-process vs. an external ACP server like
// codex-acp / claude-acp). It's rendered in the Sidebar header — the
// model + effort picker stays scoped to a single chat.
//
// The component hides itself when only the built-in wingman backend is
// configured (no external agents in ~/.wingman/agents.json), so a
// default install doesn't grow an unused dropdown.
export function AgentPicker({ subscribe }: Props) {
	const [agents, setAgents] = useState<AgentInfo[]>([]);
	const [current, setCurrent] = useState(BUILTIN_AGENT_ID);
	const [open, setOpen] = useState(false);
	const popRef = useRef<HTMLDivElement>(null);
	const btnRef = useRef<HTMLButtonElement>(null);

	const load = useCallback(() => {
		fetch("/api/agents")
			.then((r) => r.json())
			.then((data: AgentInfo[]) => setAgents(data))
			.catch(() => setAgents([]));
		fetch("/api/agent")
			.then((r) => r.json())
			.then((data) => setCurrent(data.agent || BUILTIN_AGENT_ID))
			.catch(() => {});
	}, []);

	useEffect(() => {
		load();
	}, [load]);

	// React to agent_changed broadcasts so another tab switching the
	// backend updates this picker too.
	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "agent_changed") {
				load();
			}
		});
	}, [subscribe, load]);

	useEffect(() => {
		if (!open) return;
		const handler = (e: MouseEvent) => {
			const target = e.target as Node;
			if (
				popRef.current &&
				!popRef.current.contains(target) &&
				btnRef.current &&
				!btnRef.current.contains(target)
			) {
				setOpen(false);
			}
		};
		document.addEventListener("mousedown", handler);
		return () => document.removeEventListener("mousedown", handler);
	}, [open]);

	const select = useCallback((id: string) => {
		fetch("/api/agent", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ agent: id }),
		})
			.then(async (r) => {
				if (!r.ok) throw new Error(await r.text());
				return r.json();
			})
			.then((data) => setCurrent(data.agent || id))
			.catch(() => {})
			.finally(() => setOpen(false));
	}, []);

	const currentName = useMemo(() => {
		const match = agents.find((a) => a.id === current);
		return match?.name || current;
	}, [agents, current]);

	// Single-agent installs see no value in the dropdown — hide it.
	if (agents.length <= 1) return null;

	return (
		<div className="relative">
			<button
				ref={btnRef}
				type="button"
				onClick={() => setOpen((v) => !v)}
				className="flex items-center gap-1 px-2 h-7 rounded text-[11.5px] text-fg-muted hover:text-fg hover:bg-bg-hover cursor-pointer transition-colors max-w-[180px]"
				title={`Agent: ${currentName}`}
			>
				<Bot size={12} className="shrink-0" />
				<span className="truncate">{currentName}</span>
				<ChevronDown size={10} className="shrink-0 text-fg-dim" />
			</button>
			{open && (
				<div
					ref={popRef}
					className="absolute top-full mt-1 left-0 min-w-[180px] max-w-[260px] bg-bg-elevated/95 backdrop-blur-sm border border-border rounded-md shadow-xl z-50"
				>
					<div className="py-1 max-h-[260px] overflow-y-auto">
						{agents.map((a) => (
							<button
								type="button"
								key={a.id}
								className={`block w-full text-left px-3 py-1.5 text-[12px] cursor-pointer whitespace-nowrap transition-colors ${
									a.id === current
										? "text-fg bg-bg-active"
										: "text-fg-muted hover:text-fg hover:bg-bg-hover"
								}`}
								onClick={() => select(a.id)}
							>
								{a.name}
							</button>
						))}
					</div>
				</div>
			)}
		</div>
	);
}
