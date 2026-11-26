import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { CircleAlert, Loader2 } from "lucide-react";
import { type ReactNode, useState } from "react";
import {
	mcpServersQuery,
	type McpServer,
	type McpServerState,
	type McpTool,
} from "../api/mcp";
import { pluginsQuery, type Plugin } from "../api/plugins";
import { skillCatalogueQuery, type Skill } from "../api/skills";
import { errorText } from "../utils/errorText.ts";
import { Card } from "./ui/Card";

/** How many tools a server shows before it asks. */
const TOOL_ROWS = 8;

type Tone = "ok" | "idle" | "warn" | "bad";

const STATES: Record<McpServerState, { label: string; tone: Tone }> = {
	connected: { label: "Connected", tone: "ok" },
	connecting: { label: "Connecting…", tone: "idle" },
	pending: { label: "Not started", tone: "idle" },
	failed: { label: "Failed", tone: "bad" },
	unapproved: { label: "Not approved", tone: "warn" },
	login_required: { label: "Needs login", tone: "warn" },
	disconnected: { label: "Disconnected", tone: "bad" },
};

const TONES: Record<Tone, { dot: string; text: string }> = {
	ok: { dot: "bg-success", text: "text-fg-dim" },
	idle: { dot: "border border-fg-dim", text: "text-fg-dim" },
	warn: { dot: "bg-warning", text: "text-warning" },
	bad: { dot: "bg-danger", text: "text-danger" },
};

const SKILL_SCOPES: Record<NonNullable<Skill["scope"]>, string> = {
	project: "project",
	personal: "personal",
	plugin: "plugin",
	builtin: "built-in",
};

const row = "border-b border-border-subtle/60 px-2.5 py-2 last:border-b-0";
const cell = "min-w-0 px-2.5 py-2";

const plural = (count: number, one: string, many = `${one}s`) =>
	`${count} ${count === 1 ? one : many}`;

function Badge({ children }: { children: string }) {
	return (
		<span className="shrink-0 rounded border border-border px-1 text-[9px] leading-[14px] text-fg-dim">
			{children}
		</span>
	);
}

/** A card's rows once they are read, and what it says until then. */
function Rows<T>({
	query,
	empty,
	children,
}: {
	query: UseQueryResult<T[]>;
	empty: string;
	children: (items: T[]) => ReactNode;
}) {
	const note = "px-3 py-4 text-center text-[11px] text-fg-dim";
	if (query.error) return <div className={note}>{errorText(query.error)}</div>;
	if (!query.data)
		return (
			<div className={`${note} flex items-center justify-center gap-1.5`}>
				<Loader2 size={11} className="animate-spin" />
				Reading…
			</div>
		);
	if (query.data.length === 0) return <div className={note}>{empty}</div>;
	return children(query.data);
}

function ToolRow({ tool }: { tool: McpTool }) {
	const [open, setOpen] = useState(false);
	return (
		<li>
			<button
				type="button"
				disabled={!tool.description}
				aria-expanded={tool.description ? open : undefined}
				onClick={() => setOpen(!open)}
				className="-mx-1.5 grid w-[calc(100%+0.75rem)] grid-cols-1 gap-x-4 rounded px-1.5 py-0.5 text-left text-[11px] leading-[18px] transition-colors enabled:hover:bg-bg-hover @2xl:grid-cols-[14rem_minmax(0,1fr)]"
			>
				<span className="truncate font-mono text-fg-muted">{tool.name}</span>
				{tool.description && (
					<span
						className={`text-fg-dim ${open ? "break-words whitespace-pre-wrap" : "truncate"}`}
					>
						{tool.description}
					</span>
				)}
			</button>
		</li>
	);
}

function ServerRow({ server }: { server: McpServer }) {
	const [all, setAll] = useState(false);
	const state = STATES[server.state] ?? STATES.pending;
	const tone = TONES[state.tone];
	const count = server.tools.length;
	const tools = all ? server.tools : server.tools.slice(0, TOOL_ROWS);
	const hidden = count - tools.length;

	return (
		<section
			aria-label={server.name}
			data-mcp-server={server.name}
			data-state={server.state}
			className={row}
		>
			<div className="flex items-center gap-1.5">
				<span className="flex w-3 shrink-0 items-center justify-center">
					{server.state === "connecting" ? (
						<Loader2 size={10} className="animate-spin text-fg-dim" />
					) : (
						<span className={`size-1.5 rounded-full ${tone.dot}`} />
					)}
				</span>
				<span className="truncate text-[12px] font-medium text-fg">
					{server.name}
				</span>
				{server.scope && <Badge>{server.scope}</Badge>}
				{server.transport && <Badge>{server.transport}</Badge>}
				{server.target && (
					<span
						className="ml-1 hidden min-w-0 flex-1 truncate font-mono text-[10px] text-fg-dim @2xl:block"
						title={server.target}
					>
						{server.target}
					</span>
				)}
				<span className="ml-auto shrink-0 pl-2 text-[10px] text-fg-dim tabular-nums">
					<span className={tone.text}>{state.label}</span>
					{count > 0 && ` · ${plural(count, "tool")}`}
				</span>
			</div>
			<div className="pl-[18px]">
				{server.target && (
					<div
						className="mt-0.5 truncate font-mono text-[10px] text-fg-dim @2xl:hidden"
						title={server.target}
					>
						{server.target}
					</div>
				)}
				{server.error && (
					<div className="mt-1.5 flex items-start gap-1.5 text-[11px] leading-[18px] text-fg-muted">
						<CircleAlert
							size={11}
							className={`mt-[3.5px] shrink-0 ${tone.text}`}
						/>
						<span className="min-w-0 break-words">{server.error}</span>
					</div>
				)}
				{tools.length > 0 && (
					<ul className="mt-1.5">
						{tools.map((tool) => (
							<ToolRow key={tool.name} tool={tool} />
						))}
					</ul>
				)}
				{hidden > 0 && (
					<button
						type="button"
						className="-ml-1.5 rounded px-1.5 py-0.5 text-[10px] text-fg-muted transition-colors hover:bg-bg-hover hover:text-fg"
						onClick={() => setAll(true)}
					>
						Show {hidden} more
					</button>
				)}
			</div>
		</section>
	);
}

function PluginRow({ plugin }: { plugin: Plugin }) {
	const brings = [
		plugin.skills > 0 && plural(plugin.skills, "skill"),
		plugin.mcp_servers > 0 && plural(plugin.mcp_servers, "MCP server"),
		plugin.language_servers > 0 &&
			plural(plugin.language_servers, "language server"),
		plugin.hooks && "hooks",
	].filter(Boolean);
	return (
		<div data-plugin={plugin.name} className={cell}>
			<div className="flex items-center gap-1.5">
				<span className="truncate text-[12px] font-medium text-fg">
					{plugin.name}
				</span>
				{plugin.version && (
					<span className="shrink-0 font-mono text-[10px] text-fg-dim">
						{plugin.version}
					</span>
				)}
				<Badge>{plugin.scope}</Badge>
				<span className="ml-auto min-w-0 shrink-[10] truncate pl-2 text-[10px] text-fg-dim">
					{brings.join(" · ")}
				</span>
			</div>
			{plugin.description && (
				<div
					className="mt-0.5 line-clamp-2 text-[11px] leading-[16px] text-fg-dim"
					title={plugin.description}
				>
					{plugin.description}
				</div>
			)}
		</div>
	);
}

function SkillRow({ skill }: { skill: Skill }) {
	return (
		<div data-skill={skill.name} className={cell}>
			<div className="flex items-center gap-1.5">
				<span className="truncate font-mono text-[11px] text-fg-muted">
					/{skill.name}
				</span>
				{skill.scope && <Badge>{SKILL_SCOPES[skill.scope]}</Badge>}
			</div>
			{skill.description && (
				<div
					className="mt-0.5 line-clamp-2 text-[11px] leading-[16px] text-fg-dim"
					title={skill.description}
				>
					{skill.description}
				</div>
			)}
		</div>
	);
}

const SKILL_ORDER = Object.keys(SKILL_SCOPES);

/** A project's and the user's own skills come before the ones Wingman brings. */
function bySource(skills: Skill[]): Skill[] {
	const rank = (skill: Skill) => SKILL_ORDER.indexOf(skill.scope ?? "builtin");
	return skills.toSorted(
		(a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name),
	);
}

export function ConnectionsView() {
	const servers = useQuery(mcpServersQuery);
	const plugins = useQuery(pluginsQuery);
	const skills = useQuery(skillCatalogueQuery);

	const connected = servers.data?.filter(
		(server) => server.state === "connected",
	).length;
	const tools = servers.data?.reduce(
		(sum, server) => sum + server.tools.length,
		0,
	);

	return (
		<div className="@container h-full overflow-y-auto p-3">
			<div className="flex flex-col gap-3">
				<Card
					title="MCP servers"
					detail={
						servers.data?.length
							? `${connected} of ${servers.data.length} connected · ${plural(tools ?? 0, "tool")}`
							: undefined
					}
				>
					<Rows
						query={servers}
						empty="No MCP servers. Add one with “wingman mcp add”, then reopen this workspace."
					>
						{(items) =>
							items.map((server) => (
								<ServerRow key={server.name} server={server} />
							))
						}
					</Rows>
				</Card>
				<Card
					title="Plugins"
					detail={
						plugins.data?.length
							? `${plugins.data.length} installed`
							: undefined
					}
				>
					<Rows query={plugins} empty="No plugins installed">
						{(items) => (
							<div className="grid grid-cols-1 gap-x-3 @4xl:grid-cols-2">
								{items.map((plugin) => (
									<PluginRow key={plugin.name} plugin={plugin} />
								))}
							</div>
						)}
					</Rows>
				</Card>
				<Card
					title="Skills"
					detail={
						skills.data?.length ? `${skills.data.length} available` : undefined
					}
				>
					<Rows query={skills} empty="No skills">
						{(items) => (
							<div className="grid grid-cols-1 gap-x-3 @2xl:grid-cols-2 @5xl:grid-cols-3">
								{bySource(items).map((skill) => (
									<SkillRow key={skill.name} skill={skill} />
								))}
							</div>
						)}
					</Rows>
				</Card>
			</div>
		</div>
	);
}
