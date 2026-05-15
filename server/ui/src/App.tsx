import {
	FileText,
	GitCompare,
	Loader2,
	MessageSquare,
	PanelLeftClose,
	PanelLeftOpen,
	PanelRightClose,
	PanelRightOpen,
	Plus,
	X,
} from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { ChatPanel } from "./components/ChatPanel";
import { CheckpointsPanel } from "./components/CheckpointsPanel";
import { DiffsPanel } from "./components/DiffsPanel";
import { DiffTab } from "./components/DiffTab";
import { FileTab } from "./components/FileTab";
import { FileTree } from "./components/FileTree";
import { ProblemsPanel } from "./components/ProblemsPanel";
import { Sidebar } from "./components/Sidebar";
import { useCapabilities } from "./hooks/useCapabilities";
import { useWebSocket } from "./hooks/useWebSocket";

interface CenterTab {
	id: string;
	type: "chat" | "file" | "diff";
	label: string;
	path?: string;
	line?: number;
}

type RightTab = "changes" | "files";

const EMPTY_ENTRIES: never[] = [];
const EMPTY_USAGE = { inputTokens: 0, cachedTokens: 0, outputTokens: 0 };

export default function App() {
	const {
		connected,
		sessions,
		sendChat,
		cancel,
		removeSession,
		subscribe,
	} = useWebSocket();
	const capabilities = useCapabilities(subscribe);
	const showChanges = capabilities?.diffs ?? false;
	const inGitRepo = capabilities?.git ?? false;
	const showProblems = capabilities?.lsp ?? false;
	const [sessionId, setSessionId] = useState("");
	const [rightTab, setRightTab] = useState<RightTab>("changes");
	const [sidebarCollapsed, setSidebarCollapsed] = useState(true);
	const [rightPanelCollapsed, setRightPanelCollapsed] = useState(false);

	const activeSession = sessionId ? sessions[sessionId] : undefined;
	const entries = activeSession?.entries ?? EMPTY_ENTRIES;
	const phase = activeSession?.phase ?? "idle";
	const usage = activeSession?.usage ?? EMPTY_USAGE;

	// On first WS connect the server announces every in-memory session
	// (handler_chat.handleWebSocket). Latch onto whichever shows up first
	// so the user has an active session selected without a manual click.
	useEffect(() => {
		if (sessionId) return;
		const first = Object.keys(sessions)[0];
		if (first) setSessionId(first);
	}, [sessionId, sessions]);

	const [prevInGit, setPrevInGit] = useState<boolean | null>(null);
	if (capabilities && prevInGit !== inGitRepo) {
		setPrevInGit(inGitRepo);
		if (prevInGit === null) {
			if (!inGitRepo) setRightTab("files");
		} else {
			setRightTab(inGitRepo ? "changes" : "files");
		}
	}

	const [tabs, setTabs] = useState<CenterTab[]>([
		{ id: "chat", type: "chat", label: "Session" },
	]);
	const [activeTabId, setActiveTabId] = useState("chat");

	const openFile = useCallback(
		(path: string, line?: number) => {
			const existing = tabs.find((t) => t.type === "file" && t.path === path);
			if (existing) {
				if (line) {
					setTabs((prev) =>
						prev.map((t) => (t.id === existing.id ? { ...t, line } : t)),
					);
				}
				setActiveTabId(existing.id);
				return;
			}
			const label = path.split("/").pop() || path;
			const tab: CenterTab = { id: `file:${path}`, type: "file", label, path, line };
			setTabs((prev) => [...prev, tab]);
			setActiveTabId(tab.id);
		},
		[tabs],
	);

	const openDiff = useCallback(
		(path: string) => {
			const existing = tabs.find((t) => t.type === "diff" && t.path === path);
			if (existing) {
				setActiveTabId(existing.id);
				return;
			}
			const label = path.split("/").pop() || path;
			const tab: CenterTab = { id: `diff:${path}`, type: "diff", label, path };
			setTabs((prev) => [...prev, tab]);
			setActiveTabId(tab.id);
		},
		[tabs],
	);

	const closeTab = useCallback(
		(id: string) => {
			if (id === "chat") return;
			setTabs((prev) => prev.filter((t) => t.id !== id));
			if (activeTabId === id) setActiveTabId("chat");
		},
		[activeTabId],
	);

	const handleNewSession = useCallback(() => {
		// Mint the new session id locally and switch to it. Using a UUID (vs.
		// clearing sessionId) keeps the auto-pick effect inert — otherwise
		// it would land on the first existing session and the chat would
		// re-fill with old content. The server learns about this id when
		// the user actually sends a message (lazy-create in handleSend).
		setSessionId(crypto.randomUUID());
		setActiveTabId("chat");
	}, []);

	const handleSessionDeleted = useCallback(
		(id: string) => {
			removeSession(id);
			if (id === sessionId) {
				// No auto-bootstrap on the server — clear the active session
				// and let the user pick from the sidebar or type to create.
				setSessionId("");
			}
		},
		[removeSession, sessionId],
	);

	const handleSessionSelect = useCallback(
		async (id: string) => {
			// Already in memory? just switch — its state is already up to date.
			if (sessions[id]) {
				setSessionId(id);
				setActiveTabId("chat");
				return;
			}
			// Disk-only session: ask server to load it. Server registers it,
			// then pushes a session_state event via WS to populate the slot.
			const res = await fetch(`/api/sessions/${id}/load`, { method: "POST" });
			if (!res.ok) return;
			setSessionId(id);
			setActiveTabId("chat");
		},
		[sessions],
	);

	const handleSend = useCallback(
		(text: string, files?: string[], images?: string[]) => {
			// Lazy-create: when there's no active session, mint a fresh UUID
			// and use it for the send. The server adopts unknown ids on first
			// MsgSend, so this materializes the conversation on the round-trip.
			let sid = sessionId;
			if (!sid) {
				sid = crypto.randomUUID();
				setSessionId(sid);
			}
			sendChat(sid, text, files, images);
		},
		[sendChat, sessionId],
	);

	const handleCancel = useCallback(() => {
		if (sessionId) cancel(sessionId);
	}, [cancel, sessionId]);

	const activeTab = tabs.find((t) => t.id === activeTabId) || tabs[0];

	const [noticeDismissed, setNoticeDismissed] = useState(false);
	const showNotice = !!capabilities?.notice && !noticeDismissed;

	// Sessions whose phase indicates an in-flight turn — drives the sidebar's
	// running badge so the user sees which background conversations are busy.
	const runningSessionIds = new Set(
		Object.values(sessions)
			.filter((s) => s.phase !== "idle")
			.map((s) => s.id),
	);

	// "+" only makes sense when the active session has real content. Without
	// it, clicking either reuses the current empty session or has nothing
	// meaningful to do — so we hide it. The user creates the first session
	// by typing (lazy-create on send) or by picking one from the sidebar.
	const canCreateNew = !!(
		sessionId && (sessions[sessionId]?.entries.length ?? 0) > 0
	);

	return (
		<div className="relative flex flex-col h-screen bg-bg text-fg">
			{showNotice && (
				<div className="shrink-0 px-4 py-2 text-[12px] flex items-center gap-3 bg-yellow-500/10 border-b border-yellow-500/30 text-yellow-700 dark:text-yellow-300">
					<span className="flex-1">{capabilities?.notice}</span>
					<button
						type="button"
						onClick={() => setNoticeDismissed(true)}
						className="opacity-70 hover:opacity-100 px-1"
						aria-label="Dismiss"
					>
						×
					</button>
				</div>
			)}
			<div className="flex flex-1 overflow-hidden">
				{/* Left Sidebar */}
				<div
					className="shrink-0 overflow-hidden transition-[width] duration-200 ease-in-out border-r border-border-subtle"
					style={{ width: sidebarCollapsed ? 0 : 224, borderRightWidth: sidebarCollapsed ? 0 : 1 }}
				>
					<div className="w-56 h-full">
						<Sidebar
							currentSessionId={sessionId}
							onSessionSelect={handleSessionSelect}
							onNewSession={handleNewSession}
							onSessionDeleted={handleSessionDeleted}
							runningSessionIds={runningSessionIds}
							canCreateNew={canCreateNew}
							subscribe={subscribe}
						/>
					</div>
				</div>

				{/* Center Panel */}
				<div className="flex-1 flex flex-col overflow-hidden min-w-0 bg-bg">
					<div className="h-10 flex items-stretch bg-bg shrink-0 overflow-x-auto">
						<button
							type="button"
							className="self-center flex items-center justify-center w-8 h-8 ml-1 rounded-md text-fg-dim hover:text-fg-muted hover:bg-bg-hover cursor-pointer transition-colors shrink-0"
							onClick={() => setSidebarCollapsed((c) => !c)}
							title={sidebarCollapsed ? "Show sidebar" : "Hide sidebar"}
						>
							{sidebarCollapsed ? (
								<PanelLeftOpen size={13} />
							) : (
								<PanelLeftClose size={13} />
							)}
						</button>
						{sidebarCollapsed && canCreateNew && (
							<button
								type="button"
								className="self-center flex items-center justify-center w-8 h-8 rounded-md text-fg-dim hover:text-fg-muted hover:bg-bg-hover cursor-pointer transition-colors shrink-0"
								onClick={handleNewSession}
								title="New session"
							>
								<Plus size={13} />
							</button>
						)}
						{tabs.map((tab) => {
							const active = tab.id === activeTabId;
							const Icon =
								tab.type === "chat"
									? MessageSquare
									: tab.type === "diff"
										? GitCompare
										: FileText;
							return (
								<div
									key={tab.id}
									className={`group relative flex items-center gap-1.5 px-3 cursor-pointer text-[12px] shrink-0 select-none transition-colors ${
										active ? "text-fg" : "text-fg-dim hover:text-fg-muted"
									}`}
									onClick={() => setActiveTabId(tab.id)}
								>
									{active && (
										<span className="absolute bottom-0 left-2 right-2 h-[2px] bg-accent rounded-full" />
									)}
									<span className="w-3.5 h-3.5 flex items-center justify-center shrink-0">
										{tab.type === "chat" ? (
											<Icon
												size={13}
												className={active ? "text-fg-muted" : "text-fg-dim"}
											/>
										) : (
											<>
												<Icon
													size={13}
													className={`group-hover:hidden ${active ? "text-fg-muted" : "text-fg-dim"}`}
												/>
												<button
													type="button"
													className="hidden group-hover:flex w-3.5 h-3.5 items-center justify-center text-fg-dim hover:text-fg rounded transition-colors"
													onClick={(e) => {
														e.stopPropagation();
														closeTab(tab.id);
													}}
													aria-label="Close tab"
												>
													<X size={11} />
												</button>
											</>
										)}
									</span>
									<span className="truncate max-w-[200px]">{tab.label}</span>
								</div>
							);
						})}
						<div className="flex-1" />
						{(usage.inputTokens > 0 || usage.outputTokens > 0) && (
							<div className="flex items-center px-3 text-[11px] text-fg-dim tabular-nums whitespace-nowrap">
								{"↑"}
								{formatTokens(usage.inputTokens)}
								{usage.cachedTokens > 0 && (
									<span className="ml-1">({formatTokens(usage.cachedTokens)} cached)</span>
								)}
								<span className="ml-2">
									{"↓"}
									{formatTokens(usage.outputTokens)}
								</span>
							</div>
						)}
						<button
							type="button"
							className="self-center flex items-center justify-center w-8 h-8 mr-1 rounded-md text-fg-dim hover:text-fg-muted hover:bg-bg-hover cursor-pointer transition-colors shrink-0"
							onClick={() => setRightPanelCollapsed((c) => !c)}
							title={rightPanelCollapsed ? "Show panel" : "Hide panel"}
						>
							{rightPanelCollapsed ? (
								<PanelRightOpen size={13} />
							) : (
								<PanelRightClose size={13} />
							)}
						</button>
					</div>

					<div className="h-px bg-border-subtle shrink-0" />

					<div className="flex-1 overflow-hidden">
						{activeTab.type === "chat" ? (
							<ChatPanel
								key={sessionId || "no-session"}
								sessionId={sessionId}
								entries={entries}
								phase={phase}
								onSend={handleSend}
								onCancel={handleCancel}
							/>
						) : activeTab.type === "diff" && activeTab.path ? (
							<DiffTab
								path={activeTab.path}
								sessionId={sessionId}
								subscribe={subscribe}
								onDeleted={() => closeTab(activeTab.id)}
							/>
						) : activeTab.path ? (
							<FileTab
								path={activeTab.path}
								line={activeTab.line}
								subscribe={subscribe}
								onDeleted={() => closeTab(activeTab.id)}
							/>
						) : null}
					</div>
				</div>

				{/* Right Panel */}
				<div
					className="shrink-0 overflow-hidden transition-[width] duration-200 ease-in-out border-l border-border-subtle"
					style={{ width: rightPanelCollapsed ? 0 : 288, borderLeftWidth: rightPanelCollapsed ? 0 : 1 }}
				>
					<div className="w-72 h-full flex flex-col bg-bg">
						<div className="h-10 flex items-stretch shrink-0">
							{showChanges && (
								<RightTabButton
									active={rightTab === "changes"}
									onClick={() => setRightTab("changes")}
								>
									Changes
								</RightTabButton>
							)}
							<RightTabButton
								active={rightTab === "files"}
								onClick={() => setRightTab("files")}
							>
								Files
							</RightTabButton>
							<div className="flex-1" />
						</div>
						<div className="h-px bg-border-subtle shrink-0" />
						<div className="flex-1 overflow-hidden">
							{rightTab === "changes" && showChanges ? (
								<div className="flex flex-col h-full">
									<div className="flex-[3] min-h-0 overflow-hidden">
										<DiffsPanel
											sessionId={sessionId}
											onOpenDiff={openDiff}
											onOpenFile={openFile}
											subscribe={subscribe}
										/>
									</div>
									<div className="h-px bg-border-subtle shrink-0" />
									<div className="flex-[1] min-h-0 overflow-hidden">
										<CheckpointsPanel sessionId={sessionId} subscribe={subscribe} />
									</div>
								</div>
							) : (
								<div className="flex flex-col h-full">
									<div className="flex-[3] min-h-0 overflow-hidden flex flex-col">
										<FileTree onFileSelect={openFile} subscribe={subscribe} />
									</div>
									{showProblems && (
										<>
											<div className="h-px bg-border-subtle shrink-0" />
											<div className="flex-[1] min-h-0 overflow-hidden">
												<ProblemsPanel
													onOpenFile={openFile}
													subscribe={subscribe}
												/>
											</div>
										</>
									)}
								</div>
							)}
						</div>
					</div>
				</div>
			</div>

			{!connected && (
				<div className="absolute inset-0 z-50 flex items-center justify-center backdrop-blur-md bg-bg/60">
					<div className="flex flex-col items-center gap-3 text-fg-muted">
						<Loader2 size={28} className="animate-spin" />
						<div className="text-[13px]">Reconnecting…</div>
					</div>
				</div>
			)}
		</div>
	);
}

function formatTokens(n: number): string {
	if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
	if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
	return String(n);
}

function RightTabButton({
	active,
	onClick,
	children,
}: {
	active: boolean;
	onClick: () => void;
	children: React.ReactNode;
}) {
	return (
		<button
			type="button"
			className={`relative px-4 text-[11px] font-medium cursor-pointer transition-colors ${
				active ? "text-fg" : "text-fg-dim hover:text-fg-muted"
			}`}
			onClick={onClick}
		>
			{children}
			{active && (
				<span className="absolute left-3 right-3 bottom-0 h-[2px] bg-accent rounded-full" />
			)}
		</button>
	);
}
