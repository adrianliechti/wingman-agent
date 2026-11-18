import type {
	SessionChatInputRequest,
	SessionInputRequest,
	SessionToolConfirmationRequest,
} from "@microsoft/agent-host-protocol";
import {
	Archive,
	ArchiveRestore,
	Bell,
	BellOff,
	Check,
	ChevronDown,
	ChevronRight,
	CircleAlert,
	CircleHelp,
	Eye,
	EyeOff,
	FolderOpen,
	GitFork,
	ListFilter,
	Loader2,
	MessageSquare,
	MoreHorizontal,
	Search,
	Trash2,
	X,
} from "lucide-react";
import {
	memo,
	type ReactNode,
	useCallback,
	useEffect,
	useMemo,
	useRef,
	useState,
} from "react";
import { useAgents } from "../hooks/useAgents.ts";
import {
	keepUnread,
	useFleet,
	useFleetView,
	useHere,
	useNotificationSetting,
	useNow,
	useSessionInputNeeded,
} from "../hooks/useFleet.ts";
import { useSessionAction } from "../hooks/useSessions.ts";
import { display } from "../state/ahpSession.ts";
import {
	type FleetChanges,
	type FleetChat,
	type FleetFilter,
	type FleetGroup,
	type FleetSession,
	type FleetState,
	groupFleet,
	LIVE_GROUPS,
	relativeTime,
	shownGroups,
	shownSessions,
} from "../state/fleet.ts";
import { useWorkspace } from "../state/workspaceContext.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import type { TabDisposition } from "../types/tabs";
import { formatAgentName } from "../utils/agents";
import { AgentIcon } from "./AgentIcon";
import { useToast } from "./ui/Feedback";
import { FloatingMenu } from "./ui/Floating";

interface Props {
	/** The chat on screen, which the list marks as current. */
	currentSessionId: string;
	onSessionSelect: (id: string, disposition?: TabDisposition) => void;
	onSessionDelete?: (id: string, title: string) => void;
	onSessionFork?: (id: string) => void;
	/** Shows another project in the window, by its URI. */
	onOpenProject?: (uri: string, name: string) => void;
	/** Larger targets and a menu button for phones. */
	touch?: boolean;
}

const FILTERS: { id: FleetFilter; label: string; empty: string }[] = [
	{ id: "all", label: "All sessions", empty: "No sessions yet" },
	{ id: "needs-input", label: "Needs you", empty: "Nothing needs you" },
	{ id: "running", label: "Running", empty: "Nothing is running" },
	{ id: "archived", label: "Archived", empty: "No archived sessions" },
];

/** How many sessions a group shows before it asks, and how many more each time. */
const GROUP_ROWS = 8;
const MORE_ROWS = 25;
/** How many other projects the list shows before it asks. */
const PROJECT_GROUPS = 8;
/** A heading stays in view while the list scrolls through its group. It
 * covers the rows that pass under it with the colour the side panels show:
 * the surface that lies, part see-through, on the window's background. */
const stickyHeading =
	"sticky top-0 z-[1] bg-[color-mix(in_srgb,var(--color-bg-surface)_40%,var(--color-bg))]";
const moreButton =
	"block rounded px-2 py-1 text-left text-[11px] text-fg-muted hover:bg-bg-hover hover:text-fg";

type Menu = {
	chat: FleetChat;
	session: FleetSession;
	x: number;
	y: number;
};

/** Every session of every agent: what waits for the user first, then what works, then the rest by day. */
export function SessionsPanel({
	currentSessionId,
	onSessionSelect,
	onSessionDelete,
	onSessionFork,
	onOpenProject,
	touch = false,
}: Props) {
	const client = workspaceClient();
	const toast = useToast();
	const act = useSessionAction();
	const { backend } = useWorkspace();
	const agents = useAgents();
	const { sessions, loaded, error, starting, counts } = useFleet();
	const notifications = useNotificationSetting();
	const now = useNow();
	const here = useHere();
	const [view, setView] = useFleetView();
	const { filter, provider, search, searching } = view;
	const setFilter = (next: FleetFilter) => setView({ filter: next });
	const setProvider = (next: string) => setView({ provider: next });
	const setSearch = (next: string) => setView({ search: next });
	const [filterButton, setFilterButton] = useState<HTMLButtonElement | null>(
		null,
	);
	const [filterOpen, setFilterOpen] = useState(false);
	const [menu, setMenu] = useState<Menu | null>(null);
	/** The sessions each group shows beyond its first ones. */
	const [more, setMore] = useState<Readonly<Record<string, number>>>({});
	const [allProjects, setAllProjects] = useState(false);
	const searchInput = useRef<HTMLInputElement>(null);
	useEffect(() => {
		if (searching) searchInput.current?.focus();
	}, [searching]);
	const agentName = useMemo(() => {
		const names = new Map(
			agents.map((agent) => [agent.id, formatAgentName(agent.id, agent.name)]),
		);
		return (id: string) => names.get(id) ?? formatAgentName(id);
	}, [agents]);
	// An agent lists the sessions of its own CLI only once it runs, so the list
	// starts the agents it shows: every agent, or the one it is filtered to.
	const agentIds = agents.map((agent) => agent.id).join("\n");
	useEffect(() => {
		const shown = provider ? [provider] : agentIds.split("\n");
		for (const id of new Set([backend, ...shown]))
			if (id) void client.catalog.include(id).catch(() => {});
	}, [client, backend, provider, agentIds]);
	const groups = useMemo(
		() =>
			groupFleet(sessions, {
				filter,
				search,
				provider,
				providerName: agentName,
				here,
				ownProject: client.scope.project,
				now,
			}),
		[sessions, filter, search, provider, agentName, here, client, now],
	);
	const otherProject = (session: FleetSession) =>
		here(session) ? undefined : session.project?.displayName;
	// A search shows what it found in every project; the groups return to how
	// the user left them when it ends.
	const expanded = (group: FleetGroup) =>
		!!search.trim() || (view.expanded[group.id] ?? group.project?.here ?? true);
	// A row renders again only when its session changes, so what the rows
	// call keeps its identity while the panel renders.
	const select = useRef(onSessionSelect);
	useEffect(() => {
		select.current = onSessionSelect;
	});
	const openChat = useCallback(
		(id: string, disposition?: TabDisposition) =>
			select.current(id, disposition),
		[],
	);
	const openMenu = useCallback(
		(
			event: { clientX: number; clientY: number },
			session: FleetSession,
			chat: FleetChat,
		) => setMenu({ session, chat, x: event.clientX, y: event.clientY }),
		[],
	);
	const menuFork = !!menu && menu.chat.id !== menu.session.id;
	// The projects used last come first; the others wait behind one line. A
	// search shows every project that holds what it found.
	const { groups: listedGroups, hidden: hiddenProjects } = shownGroups(
		groups,
		allProjects || search.trim() ? Infinity : PROJECT_GROUPS,
	);
	const filtered = filter !== "all" || !!provider || !!search.trim();
	const emptyText = search.trim()
		? "No matching sessions"
		: (FILTERS.find((item) => item.id === filter)?.empty ?? "");
	const iconButton = `flex shrink-0 items-center justify-center rounded text-fg-dim transition-colors hover:bg-bg-hover hover:text-fg ${touch ? "size-11" : "size-6"}`;

	return (
		<nav className="flex h-full w-full flex-col" aria-label="Sessions">
			<div
				className={`flex h-9 shrink-0 items-center gap-0.5 pr-1.5 ${searching ? "pl-1.5" : "pl-3"}`}
			>
				{/* The search takes the place of the title while it is open. */}
				{searching ? (
					<input
						ref={searchInput}
						value={search}
						onChange={(event) => setSearch(event.target.value)}
						onKeyDown={(event) => {
							if (event.key !== "Escape") return;
							event.stopPropagation();
							setView({ searching: false, search: "" });
						}}
						placeholder="Search sessions"
						aria-label="Search sessions"
						className={`mr-1 min-w-0 flex-1 rounded-md border border-border bg-bg-input px-2 text-[12px] text-fg outline-none placeholder:text-fg-dim focus:border-border-strong ${touch ? "h-9" : "h-6"}`}
					/>
				) : (
					<>
						<span className="text-[10px] font-medium tracking-wide text-fg-dim uppercase">
							Sessions
						</span>
						{loaded && starting.length > 0 && (
							<Loader2
								size={11}
								className="ml-1.5 animate-spin text-fg-dim"
								role="status"
								aria-label={`Listing sessions of ${starting.map(agentName).join(", ")}`}
							/>
						)}
						<div className="flex-1" />
					</>
				)}
				<button
					type="button"
					className={iconButton}
					onClick={() => {
						setView({ searching: !searching, search: "" });
					}}
					title={searching ? "Close search" : "Search sessions"}
					aria-label={searching ? "Close search" : "Search sessions"}
				>
					{searching ? (
						<X size={touch ? 18 : 12} />
					) : (
						<Search size={touch ? 18 : 12} />
					)}
				</button>
				<button
					ref={setFilterButton}
					type="button"
					className={`${iconButton} ${filter !== "all" || provider ? "text-fg" : ""}`}
					onClick={() => setFilterOpen((value) => !value)}
					title="Filter sessions"
					aria-label="Filter sessions"
					aria-haspopup="menu"
					aria-expanded={filterOpen}
				>
					<ListFilter size={touch ? 18 : 12} />
				</button>
				{notifications.supported && (
					<button
						type="button"
						className={iconButton}
						onClick={() =>
							void notifications.set(!notifications.enabled).then((done) => {
								if (!done)
									toast({
										title: "Notifications are blocked",
										description:
											"Allow notifications for this site in the browser settings.",
										tone: "error",
									});
							})
						}
						title={
							notifications.enabled
								? "Turn off notifications"
								: "Notify me when a session needs me or finishes"
						}
						aria-label={
							notifications.enabled
								? "Turn off notifications"
								: "Turn on notifications"
						}
						aria-pressed={notifications.enabled}
					>
						{notifications.enabled ? (
							<Bell size={touch ? 18 : 12} />
						) : (
							<BellOff size={touch ? 18 : 12} />
						)}
					</button>
				)}
			</div>
			{(filter !== "all" || provider) && (
				<div className="flex shrink-0 flex-wrap items-center gap-1 px-2 pb-1.5">
					{filter !== "all" && (
						<FilterChip
							label={FILTERS.find((item) => item.id === filter)?.label ?? ""}
							onClear={() => setFilter("all")}
						/>
					)}
					{provider && (
						<FilterChip
							label={agentName(provider)}
							onClear={() => setProvider("")}
						/>
					)}
				</div>
			)}
			<div className="min-h-0 flex-1 overflow-y-auto pb-2">
				{!loaded && !error && (
					<div className="flex h-full items-center justify-center">
						<Loader2 size={14} className="animate-spin text-fg-dim" />
					</div>
				)}
				{error && !loaded && (
					<div
						role="alert"
						className="px-3 py-8 text-center text-sm text-danger"
					>
						<p>Could not load sessions.</p>
						<button
							type="button"
							className="mt-2 min-h-11 rounded-md border border-border px-4"
							onClick={() => void client.catalog.reload().catch(() => {})}
						>
							Retry
						</button>
					</div>
				)}
				{loaded && groups.length === 0 && (
					<div className="px-3 py-8 text-center text-[11px] text-fg-dim">
						{emptyText}
						{filtered && (
							<button
								type="button"
								className="mt-2 block w-full text-fg-muted underline-offset-2 hover:underline"
								onClick={() =>
									setView({ filter: "all", provider: "", search: "" })
								}
							>
								Show all sessions
							</button>
						)}
					</div>
				)}
				{loaded &&
					listedGroups.map((group, index) => {
						const open = expanded(group);
						const rows = open
							? shownSessions(
									group,
									GROUP_ROWS + (more[group.id] ?? 0),
									currentSessionId,
								)
							: [];
						const hidden = group.sessions.length - rows.length;
						return (
							<section key={group.id} aria-label={group.label}>
								{group.project ? (
									<ProjectHeading
										group={group}
										first={index === 0}
										expanded={open}
										touch={touch}
										onToggle={() =>
											setView({
												expanded: { ...view.expanded, [group.id]: !open },
											})
										}
										onOpen={
											!group.project.here && onOpenProject
												? () => onOpenProject(group.project!.uri, group.label)
												: undefined
										}
									/>
								) : (
									<h3
										className={`${stickyHeading} flex items-center gap-1.5 pr-3 pb-1 pl-4 text-[10px] font-medium tracking-wider uppercase ${index === 0 ? "pt-1" : "pt-3"} ${group.id === "needs-input" ? "text-warning" : "text-fg-dim"}`}
									>
										{group.label}
										{LIVE_GROUPS.has(group.id) && (
											<span className="font-normal tabular-nums">
												{group.sessions.length}
											</span>
										)}
									</h3>
								)}
								{rows.map((session) => (
									<SessionRow
										key={session.session}
										session={session}
										current={currentSessionId}
										touch={touch}
										now={now}
										agent={
											agents.length > 1 || session.provider !== "wingman"
												? agentName(session.provider)
												: undefined
										}
										// A project's group names it already.
										project={group.project ? undefined : otherProject(session)}
										onOpen={openChat}
										onMenu={openMenu}
									/>
								))}
								{open && hidden > 0 && (
									<button
										type="button"
										className={`${moreButton} ml-7`}
										onClick={() =>
											setMore({
												...more,
												[group.id]: (more[group.id] ?? 0) + MORE_ROWS,
											})
										}
									>
										Show {Math.min(hidden, MORE_ROWS)} more
										{hidden > MORE_ROWS && (
											<span className="text-fg-dim"> of {hidden}</span>
										)}
									</button>
								)}
							</section>
						);
					})}
				{loaded && hiddenProjects > 0 && (
					<button
						type="button"
						className={`${moreButton} mt-2 ml-2`}
						onClick={() => setAllProjects(true)}
					>
						Show {hiddenProjects} more{" "}
						{hiddenProjects === 1 ? "project" : "projects"}
					</button>
				)}
			</div>
			<FloatingMenu
				open={filterOpen}
				onOpenChange={setFilterOpen}
				reference={filterButton}
				placement="bottom-end"
				label="Filter sessions"
				className="z-[100] min-w-[180px] rounded-md border border-border bg-bg-elevated py-1 shadow-xl"
			>
				{FILTERS.map((item) => (
					<MenuRadio
						key={item.id}
						checked={filter === item.id}
						label={item.label}
						count={
							item.id === "needs-input"
								? counts.needsInput
								: item.id === "running"
									? counts.working
									: undefined
						}
						onSelect={() => {
							setFilter(item.id);
							setFilterOpen(false);
						}}
					/>
				))}
				{agents.length > 1 && (
					<>
						<div className="my-1 border-t border-border-subtle" />
						<MenuRadio
							checked={!provider}
							label="All agents"
							onSelect={() => {
								setProvider("");
								setFilterOpen(false);
							}}
						/>
						{agents.map((agent) => (
							<MenuRadio
								key={agent.id}
								checked={provider === agent.id}
								label={agentName(agent.id)}
								onSelect={() => {
									setProvider(agent.id);
									setFilterOpen(false);
								}}
							/>
						))}
					</>
				)}
			</FloatingMenu>
			{menu && (
				<FloatingMenu
					open
					onOpenChange={(open) => !open && setMenu(null)}
					reference={{ x: menu.x, y: menu.y }}
					label={`Actions for ${menu.chat.title}`}
					className="z-[100] min-w-[160px] rounded-md border border-border bg-bg-elevated py-1 shadow-xl"
				>
					<MenuItem
						icon={<MessageSquare size={12} />}
						label={menuFork ? "Open fork" : "Open session"}
						onSelect={() => {
							onSessionSelect(menu.chat.id, "keep");
							setMenu(null);
						}}
					/>
					<MenuItem
						icon={menu.chat.unread ? <Eye size={12} /> : <EyeOff size={12} />}
						label={menu.chat.unread ? "Mark as read" : "Mark as unread"}
						onSelect={() => {
							const { chat, session } = menu;
							setMenu(null);
							if (!chat.unread) keepUnread(chat.id);
							void act(() =>
								client.markRead(
									menuFork ? chat.id : session.session,
									chat.unread,
								),
							);
						}}
					/>
					{!menuFork && (
						<MenuItem
							icon={
								menu.session.archived ? (
									<ArchiveRestore size={12} />
								) : (
									<Archive size={12} />
								)
							}
							label={menu.session.archived ? "Unarchive" : "Archive"}
							onSelect={() => {
								const { session } = menu;
								setMenu(null);
								void act(() =>
									client.archive(session.session, !session.archived),
								);
							}}
						/>
					)}
					{onSessionFork &&
						client.canFork(menu.session.provider) &&
						menu.chat.state !== "working" &&
						menu.chat.state !== "needs-input" && (
							<MenuItem
								icon={<GitFork size={12} />}
								label="Fork chat"
								onSelect={() => {
									onSessionFork(menu.chat.id);
									setMenu(null);
								}}
							/>
						)}
					{onSessionDelete && (
						<>
							<div className="my-1 border-t border-border-subtle" />
							<MenuItem
								icon={<Trash2 size={12} />}
								label={menuFork ? "Delete fork" : "Delete session"}
								danger
								onSelect={() => {
									onSessionDelete(menu.chat.id, menu.chat.title);
									setMenu(null);
								}}
							/>
						</>
					)}
				</FloatingMenu>
			)}
		</nav>
	);
}

/** A project's group: it opens and closes, and another project's opens the window there. */
function ProjectHeading({
	group,
	first,
	expanded,
	touch,
	onToggle,
	onOpen,
}: {
	group: FleetGroup;
	first: boolean;
	expanded: boolean;
	touch: boolean;
	onToggle: () => void;
	onOpen?: () => void;
}) {
	const unread = group.sessions.some(
		(session) => session.unread || session.forks.some((fork) => fork.unread),
	);
	const Chevron = expanded ? ChevronDown : ChevronRight;
	return (
		<div
			className={`${stickyHeading} group/project flex items-center gap-1 pr-1.5 pl-2 ${first ? "" : "mt-0.5"}`}
		>
			<button
				type="button"
				className={`flex min-w-0 flex-1 items-center gap-1.5 rounded py-1 text-left text-[12px] hover:text-fg ${group.project?.here ? "font-medium text-fg" : "text-fg-muted"} ${touch ? "min-h-11" : ""}`}
				onClick={onToggle}
				aria-expanded={expanded}
				title={group.project?.uri ? decodeURI(group.project.uri) : group.label}
			>
				<Chevron
					size={12}
					className="shrink-0 text-fg-dim"
					aria-hidden="true"
				/>
				<h3 className="min-w-0 truncate">{group.label}</h3>
				<span className="text-[10px] text-fg-dim tabular-nums">
					{group.sessions.length}
				</span>
				{!expanded && unread && (
					<span
						className="size-1.5 shrink-0 rounded-full bg-info"
						role="img"
						aria-label="Unread"
					/>
				)}
			</button>
			{onOpen && (
				<button
					type="button"
					// A quiet icon, which shows with the pointer or the keyboard on its
					// project, and always where there is neither.
					className={`flex shrink-0 items-center justify-center rounded text-fg-dim hover:bg-bg-hover hover:text-fg focus-visible:opacity-100 ${touch ? "size-11" : "size-6 opacity-0 group-focus-within/project:opacity-100 group-hover/project:opacity-100"}`}
					onClick={onOpen}
					aria-label={`Open project ${group.label}`}
					title={`Open ${group.label} in this window`}
				>
					<FolderOpen size={touch ? 18 : 12} aria-hidden="true" />
				</button>
			)}
		</div>
	);
}

function FilterChip({
	label,
	onClear,
}: {
	label: string;
	onClear: () => void;
}) {
	return (
		<span className="flex items-center gap-1 rounded-full bg-bg-active py-0.5 pr-1 pl-2 text-[11px] text-fg-muted">
			{label}
			<button
				type="button"
				className="flex size-4 items-center justify-center rounded-full hover:bg-bg-hover hover:text-fg"
				onClick={onClear}
				aria-label={`Clear filter ${label}`}
			>
				<X size={10} />
			</button>
		</span>
	);
}

function MenuRadio({
	checked,
	label,
	count,
	onSelect,
}: {
	checked: boolean;
	label: string;
	count?: number;
	onSelect: () => void;
}) {
	return (
		<button
			type="button"
			role="menuitemradio"
			aria-checked={checked}
			onClick={onSelect}
			className={`flex w-full cursor-pointer items-center gap-2 px-3 py-1.5 text-left text-[12px] transition-colors ${checked ? "bg-bg-active text-fg" : "text-fg-muted hover:bg-bg-hover hover:text-fg"}`}
		>
			<span className="flex-1 truncate">{label}</span>
			{!!count && <span className="text-fg-dim tabular-nums">{count}</span>}
			{checked && <Check size={12} className="shrink-0" />}
		</button>
	);
}

function MenuItem({
	icon,
	label,
	danger,
	onSelect,
}: {
	icon: ReactNode;
	label: string;
	danger?: boolean;
	onSelect: () => void;
}) {
	return (
		<button
			type="button"
			role="menuitem"
			onClick={onSelect}
			className={`flex w-full cursor-pointer items-center gap-2 px-3 py-1.5 text-[12px] text-fg-muted transition-colors hover:bg-bg-hover ${danger ? "hover:text-danger" : "hover:text-fg"}`}
		>
			<span className="shrink-0">{icon}</span>
			{label}
		</button>
	);
}

function StatusIcon({
	state,
	unread,
	fork,
	agent,
}: {
	state: FleetState;
	unread: boolean;
	fork?: boolean;
	/** The agent of a session, when the list holds those of several. */
	agent?: { id: string; name: string };
}) {
	switch (state) {
		case "needs-input":
			return (
				<CircleHelp
					size={12}
					className="shrink-0 text-warning"
					aria-label="Needs input"
				/>
			);
		case "working":
			return (
				<Loader2
					size={12}
					className="shrink-0 animate-spin text-accent"
					aria-label="Working"
				/>
			);
		case "failed":
			return (
				<CircleAlert
					size={12}
					className={`shrink-0 ${unread ? "text-danger" : "text-danger/60"}`}
					aria-label="Failed"
				/>
			);
	}
	if (unread)
		return (
			<span
				className="flex size-3 shrink-0 items-center justify-center"
				role="img"
				aria-label="Unread"
			>
				<span className="size-1.5 rounded-full bg-info" />
			</span>
		);
	// A session at rest shows whose it is, which a second line used to say.
	// The row names the agent in words after its title.
	if (agent && !fork)
		return (
			<span
				className="flex size-3 shrink-0 items-center justify-center opacity-70"
				aria-hidden="true"
			>
				<AgentIcon id={agent.id} size={12} />
			</span>
		);
	const Icon = fork ? GitFork : MessageSquare;
	return <Icon size={12} className="shrink-0 text-fg-dim" aria-hidden="true" />;
}

function ChangeCount({ changes }: { changes?: FleetChanges }) {
	if (!changes || (!changes.additions && !changes.deletions)) return null;
	return (
		<span
			className="shrink-0 tabular-nums"
			title={`${changes.files} ${changes.files === 1 ? "file" : "files"} changed`}
		>
			<span className="text-success">+{changes.additions}</span>{" "}
			<span className="text-danger">−{changes.deletions}</span>
		</span>
	);
}

/** A session with its forks. The list holds thousands and the host reports
 * every step of those that work: a row renders again when its own changes. */
const SessionRow = memo(function SessionRow({
	session,
	current,
	touch,
	now,
	agent,
	project,
	onOpen,
	onMenu,
}: {
	session: FleetSession;
	current: string;
	touch: boolean;
	now: Date;
	agent?: string;
	project?: string;
	onOpen: (id: string, disposition?: TabDisposition) => void;
	onMenu: (
		event: { clientX: number; clientY: number },
		session: FleetSession,
		chat: FleetChat,
	) => void;
}) {
	const busy = session.state === "working" || session.state === "needs-input";
	const detail = busy
		? session.activity ||
			(session.state === "needs-input" ? "Needs your input" : "Working…")
		: session.state === "failed"
			? "Failed"
			: undefined;
	const origin = [agent, project].filter(Boolean).join(" · ");
	return (
		<div
			className="mx-1.5"
			data-session-row={session.session}
			data-session-state={session.sessionState}
		>
			<ChatRow
				chat={session}
				session={session}
				agent={agent ? { id: session.provider, name: agent } : undefined}
				active={session.id === current}
				touch={touch}
				onOpen={onOpen}
				onMenu={onMenu}
			>
				<div className="flex min-w-0 items-baseline gap-2">
					<span
						className={`min-w-0 flex-1 truncate text-[12px] leading-snug ${session.unread && !busy ? "font-semibold text-fg" : ""}`}
					>
						{session.title}
					</span>
					{agent && <span className="sr-only">, {agent}</span>}
					{!busy && (
						<span className="shrink-0 text-[10px]">
							<ChangeCount changes={session.changes} />
						</span>
					)}
					<span className="shrink-0 text-[10px] text-fg-dim tabular-nums">
						{relativeTime(session.modifiedAt, now)}
					</span>
				</div>
				{detail && (
					<div className="mt-0.5 flex min-w-0 items-center gap-1.5 text-[11px] text-fg-dim">
						<span
							className={`min-w-0 truncate ${session.state === "needs-input" ? "text-warning" : session.state === "failed" ? "text-danger" : ""}`}
							title={detail}
						>
							{detail}
						</span>
						<span className="flex-1" />
						{/* What a session does must not hide where it does it: an
						    approval in the list is given for a project. */}
						{origin && (
							<span
								className="max-w-[45%] shrink-0 truncate"
								title={origin}
								data-session-origin
							>
								{project ?? agent}
							</span>
						)}
					</div>
				)}
			</ChatRow>
			{session.sessionState === "needs-input" && (
				<SessionRequests session={session} onOpen={onOpen} />
			)}
			{session.forks.map((fork) => (
				<ChatRow
					key={fork.id}
					chat={fork}
					session={session}
					fork
					active={fork.id === current}
					touch={touch}
					onOpen={onOpen}
					onMenu={onMenu}
				>
					<div className="flex min-w-0 items-center gap-2">
						<span
							className={`min-w-0 flex-1 truncate text-[12px] leading-snug ${fork.unread && fork.state === "idle" ? "font-semibold text-fg" : ""}`}
						>
							{fork.title}
						</span>
						<span className="text-[11px] text-fg-dim">
							<ChangeCount changes={fork.changes} />
						</span>
					</div>
				</ChatRow>
			))}
		</div>
	);
});

function ChatRow({
	chat,
	session,
	fork,
	agent,
	active,
	touch,
	onOpen,
	onMenu,
	children,
}: {
	chat: FleetChat;
	session: FleetSession;
	fork?: boolean;
	agent?: { id: string; name: string };
	active: boolean;
	touch: boolean;
	onOpen: (id: string, disposition?: TabDisposition) => void;
	onMenu: (
		event: { clientX: number; clientY: number },
		session: FleetSession,
		chat: FleetChat,
	) => void;
	children: ReactNode;
}) {
	return (
		<div
			data-session-id={chat.id}
			className={`group relative flex items-stretch rounded-md transition-colors ${fork ? "ml-4" : ""} ${active ? "bg-bg-active text-fg" : "text-fg-muted hover:bg-bg-hover hover:text-fg"}`}
			onContextMenu={(event) => {
				if (touch) return;
				event.preventDefault();
				onMenu(event, session, chat);
			}}
		>
			<button
				type="button"
				className={`flex min-w-0 flex-1 items-start gap-2 rounded-md px-2 text-left ${touch ? "min-h-11 py-2" : "py-1"}`}
				onClick={() => onOpen(chat.id, touch ? "keep" : "preview")}
				onDoubleClick={() => onOpen(chat.id, "keep")}
				onKeyDown={(event) => {
					if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
						event.preventDefault();
						onOpen(chat.id, "keep");
					} else if (
						event.key === "ContextMenu" ||
						(event.key === "F10" && event.shiftKey)
					) {
						event.preventDefault();
						const box = event.currentTarget.getBoundingClientRect();
						onMenu(
							{ clientX: box.left + 16, clientY: box.bottom },
							session,
							chat,
						);
					}
				}}
				title={agent ? `${chat.title} · ${agent.name}` : chat.title}
				aria-current={active ? "page" : undefined}
			>
				<span className="mt-[3px] flex">
					<StatusIcon
						state={chat.state}
						unread={chat.unread}
						fork={fork}
						agent={agent}
					/>
				</span>
				<div className="min-w-0 flex-1">{children}</div>
			</button>
			{touch && (
				<button
					type="button"
					className="flex min-h-11 w-11 shrink-0 items-center justify-center rounded-md text-fg-dim active:bg-bg-hover"
					aria-label={`Actions for ${chat.title}`}
					onClick={(event) => {
						const box = event.currentTarget.getBoundingClientRect();
						onMenu({ clientX: box.left, clientY: box.bottom }, session, chat);
					}}
				>
					<MoreHorizontal size={18} />
				</button>
			)}
		</div>
	);
}

type Answerable = SessionToolConfirmationRequest | SessionChatInputRequest;
/** Approvals and questions; tool runs and sign-ins are not the user's to answer here. */
const answerable = (request: SessionInputRequest): request is Answerable =>
	request.kind === "toolConfirmation" || request.kind === "chatInput";

/** The approval or question a session waits on, answerable in the list when it is a yes or no. */
function SessionRequests({
	session,
	onOpen,
}: {
	session: FleetSession;
	onOpen: (id: string, disposition?: TabDisposition) => void;
}) {
	const act = useSessionAction();
	const requests = useSessionInputNeeded(session.session).filter(answerable);
	const [busy, setBusy] = useState(false);
	const request = requests[0];
	if (!request) return null;
	const message =
		request.kind === "toolConfirmation"
			? display(request.toolCall.invocationMessage) ||
				request.toolCall.displayName
			: (request.request.message ?? "");
	const options =
		request.kind === "toolConfirmation" && "options" in request.toolCall
			? (request.toolCall.options ?? [])
			: [];
	const yesNo =
		request.kind === "toolConfirmation" || !request.request.questions?.length;
	// The client answers with the first option of the reply's kind.
	const answer = async (approve: boolean) => {
		setBusy(true);
		await act(
			(client) =>
				client.respond(request.chat, request.id, {
					action: approve ? "accept" : "decline",
				}),
			"The answer was not delivered",
		);
		setBusy(false);
	};
	const button =
		"min-h-6 rounded border px-2 text-[11px] transition-colors disabled:cursor-wait disabled:opacity-60";
	return (
		<div
			className="mr-1 mb-1 ml-6 rounded-md border border-warning/30 bg-warning/5 px-2 py-1.5"
			data-session-request={request.id}
		>
			{message && (
				<p
					className="line-clamp-3 text-[11px] break-words whitespace-pre-wrap text-fg-muted"
					title={message}
				>
					{message}
				</p>
			)}
			<div className="mt-1.5 flex flex-wrap items-center gap-1.5">
				{yesNo ? (
					<>
						<button
							type="button"
							disabled={busy}
							className={`${button} border-button-primary-border bg-button-primary text-button-primary-fg hover:bg-button-primary-hover`}
							onClick={() => void answer(true)}
						>
							{options.find((option) => option.kind === "approve")?.label ??
								"Allow"}
						</button>
						<button
							type="button"
							disabled={busy}
							className={`${button} border-border text-fg-muted hover:bg-bg-hover hover:text-fg`}
							onClick={() => void answer(false)}
						>
							{options.find((option) => option.kind === "deny")?.label ??
								"Deny"}
						</button>
					</>
				) : (
					<button
						type="button"
						className={`${button} border-border text-fg-muted hover:bg-bg-hover hover:text-fg`}
						onClick={() => onOpen(request.chat, "keep")}
					>
						Answer…
					</button>
				)}
				{requests.length > 1 && (
					<button
						type="button"
						className="text-[11px] text-fg-dim hover:text-fg"
						onClick={() => onOpen(request.chat, "keep")}
					>
						+{requests.length - 1} more
					</button>
				)}
			</div>
		</div>
	);
}
