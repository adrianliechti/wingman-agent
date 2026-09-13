import { Check } from "lucide-react";
import { useMemo, useState } from "react";
import { useWorkspace } from "../state/workspaceContext.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { formatAgentName } from "../utils/agents";
import { AgentIcon } from "./AgentIcon";
import { FloatingMenu } from "./ui/Floating";

export const BUILTIN_AGENT_ID = "wingman";

interface Props {
	onSelect: (id: string) => void | Promise<void>;
	currentId?: string;
	newChat?: boolean;
	disabled?: boolean;
}
export function AgentPicker({ onSelect, currentId, newChat, disabled }: Props) {
	const { backend } = useWorkspace();
	const current = currentId ?? backend;
	const agents = workspaceClient().scope.backends;
	const [open, setOpen] = useState(false);
	const [button, setButton] = useState<HTMLButtonElement | null>(null);
	const toggleOpen = () => setOpen((value) => !value);
	const select = (id: string) => {
		setOpen(false);
		if (id !== current) onSelect(id);
	};
	const displayedName = useMemo(() => {
		const id = current;
		const match = agents.find((a) => a.id === id);
		return formatAgentName(id, match?.name);
	}, [agents, current]);

	if (agents.length <= 1) return null;

	return (
		<div data-composer-harness className="relative shrink-0">
			<button
				ref={setButton}
				type="button"
				onClick={toggleOpen}
				disabled={disabled}
				className="picker-control flex size-7 cursor-pointer items-center justify-center rounded text-fg-dim transition-colors hover:bg-bg-hover hover:text-fg disabled:cursor-wait disabled:opacity-70"
				title={`Harness: ${displayedName}`}
				aria-label={`Harness: ${displayedName}`}
				aria-haspopup="menu"
				aria-expanded={open}
			>
				<AgentIcon id={current} />
			</button>
			<FloatingMenu
				open={open}
				onOpenChange={setOpen}
				reference={button}
				placement="top-start"
				label="Harness"
				className="z-[100] min-w-[180px] max-w-[260px] bg-bg-elevated/95 backdrop-blur-sm border border-border rounded-md shadow-xl"
			>
				<div className="py-1 max-h-[260px] overflow-y-auto">
					{agents.map((a) => (
						<button
							type="button"
							role="menuitemradio"
							aria-checked={a.id === current}
							key={a.id}
							className={`picker-control flex w-full items-center gap-2 text-left px-3 py-1.5 text-[12px] cursor-pointer transition-colors ${
								a.id === current
									? "text-fg bg-bg-active"
									: "text-fg-muted hover:text-fg hover:bg-bg-hover"
							}`}
							onClick={() => select(a.id)}
						>
							<AgentIcon id={a.id} className="shrink-0" />
							<span className="flex-1 truncate">
								{formatAgentName(a.id, a.name)}
							</span>
							{a.id === current && <Check size={12} className="shrink-0" />}
						</button>
					))}
				</div>
				{newChat && (
					<div className="border-t border-border px-3 py-2 text-[11px] text-fg-dim">
						Switching harness opens a new chat.
					</div>
				)}
			</FloatingMenu>
		</div>
	);
}
