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
import { useCallback, useEffect, useRef, useState } from "react";
import {
	Group,
	Panel,
	type PanelImperativeHandle,
	Separator,
	useDefaultLayout,
	usePanelRef,
} from "react-resizable-panels";
import { ChatPanel } from "./components/ChatPanel";
import type { ModeOption } from "./components/ModePicker";
import { CheckpointsPanel } from "./components/CheckpointsPanel";
import { DiffsPanel } from "./components/DiffsPanel";
import { DiffTab } from "./components/DiffTab";
import { FileTab } from "./components/FileTab";
import { FileTree } from "./components/FileTree";
import { ProblemsPanel } from "./components/ProblemsPanel";
import { Sidebar } from "./components/Sidebar";
import { useCapabilities } from "./hooks/useCapabilities";
import { type ChatEntry, useWebSocket } from "./hooks/useWebSocket";

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
		respondPrompt,
		removeSession,
		clearSessions,
		subscribe,
	} = useWebSocket();
	const capabilities = useCapabilities(subscribe);
	const showChanges = capabilities?.diffs ?? false;
	const showProblems = capabilities?.lsp ?? false;
	const firstSessionId = Object.keys(sessions)[0] ?? "";
	const [selectedSessionId, setSelectedSessionId] = useState("");
	const sessionId = selectedSessionId || firstSessionId;
	const [requestedRightTab, setRequestedRightTab] =
		useState<RightTab>("changes");
	const rightTab = showChanges ? requestedRightTab : "files";
	const [sidebarCollapsed, setSidebarCollapsed] = useState(true);
	const [rightPanelCollapsed, setRightPanelCollapsed] = useState(false);
	const leftPanelRef = usePanelRef();
	const rightPanelRef = usePanelRef();
	const { defaultLayout, onLayoutChanged } = useDefaultLayout({
		id: "wingman-layout",
	});

	const activeSession = sessionId ? sessions[sessionId] : undefined;
	const entries = activeSession?.entries ?? EMPTY_ENTRIES;
	const phase = activeSession?.phase ?? "idle";
	const usage = activeSession?.usage ?? EMPTY_USAGE;
	const prompt = activeSession?.prompt ?? null;

	// Usage is only reported when a request completes, so a long in-flight
	// response would show a frozen count. While streaming, fold in a rough
	// estimate of the output produced so far (rendered with ~); it snaps to the
	// exact value the moment the turn lands.
	const streamEstimate = phase !== "idle" ? estimateStreamingTokens(entries) : 0;
	const outputTokens = usage.outputTokens + streamEstimate;

	const handlePromptReply = useCallback(
		(reply: { text?: string; approved?: boolean }) => {
			if (sessionId && prompt) {
				respondPrompt(sessionId, prompt.id, reply);
			}
		},
		[respondPrompt, sessionId, prompt],
	);

	// On agent swap the prior backend's sessions are stale; drop the
	// cache and immediately allocate a fresh session against the new
	// agent so the chat tab is ready (and the ModelPicker has a
	// populated catalog for ACP backends).
	useEffect(() => {
		if (!subscribe) return;
		return subscribe(async (msg) => {
			if (msg.type !== "agent_changed") return;
			setSelectedSessionId("");
			clearSessions();
			try {
				const res = await fetch("/api/sessions", { method: "POST" });
				if (!res.ok) return;
				const data = (await res.json()) as { id?: string };
				if (!data.id) return;
				setSelectedSessionId(data.id);
			} catch {
				// leave empty; user can click +
			}
		});
	}, [subscribe, clearSessions]);

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
			const tab: CenterTab = {
				id: `file:${path}`,
				type: "file",
				label,
				path,
				line,
			};
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

	// Track unsaved-edit state per tab id so we can show a dirty indicator.
	const [dirtyTabs, setDirtyTabs] = useState<Set<string>>(() => new Set());
	const setTabDirty = useCallback((id: string, dirty: boolean) => {
		setDirtyTabs((prev) => {
			const has = prev.has(id);
			if (has === dirty) return prev;
			const next = new Set(prev);
			if (dirty) next.add(id);
			else next.delete(id);
			return next;
		});
	}, []);

	const handleNewSession = useCallback(async () => {
		try {
			const res = await fetch("/api/sessions", { method: "POST" });
			if (!res.ok) return;
			const data = (await res.json()) as { id?: string };
			if (!data.id) return;
			setSelectedSessionId(data.id);
			setActiveTabId("chat");
		} catch {
			// leave session alone
		}
	}, []);

	const handleSessionDeleted = useCallback(
		(id: string) => {
			removeSession(id);
			if (id === sessionId) {
				setSelectedSessionId("");
			}
		},
		[removeSession, sessionId],
	);

	const [loadingSession, setLoadingSession] = useState(false);
	const [loadError, setLoadError] = useState<string | null>(null);
	// Monotonic token: clicking session B while A is still loading must not let
	// A's (faster) completion clear B's loader or post a stale error.
	const loadReqRef = useRef(0);

	const handleSessionSelect = useCallback(
		async (id: string) => {
			setLoadError(null);
			setSelectedSessionId(id);
			setActiveTabId("chat");
			// Already in client state (live or previously loaded) — no fetch needed.
			if (sessions[id]) return;
			const req = ++loadReqRef.current;
			setLoadingSession(true);
			let error: string | null = null;
			try {
				const res = await fetch(`/api/sessions/${id}/load`, { method: "POST" });
				if (!res.ok) {
					error = (await res.text()).trim() || `Failed to load session (${res.status}).`;
				}
			} catch {
				error = "Failed to load session.";
			}
			// A newer selection superseded this one — let it own the loader/error.
			if (loadReqRef.current !== req) return;
			setLoadingSession(false);
			setLoadError(error);
		},
		[sessions],
	);

	const ensureSessionId = useCallback(async (): Promise<string> => {
		if (sessionId) return sessionId;
		const res = await fetch("/api/sessions", { method: "POST" });
		if (!res.ok) throw new Error("failed to allocate session");
		const data = (await res.json()) as { id?: string };
		if (!data.id) throw new Error("session id missing in response");
		setSelectedSessionId(data.id);
		return data.id;
	}, [sessionId]);

	const handleSend = useCallback(
		async (text: string, files?: string[], images?: string[]) => {
			try {
				const sid = await ensureSessionId();
				sendChat(sid, text, files, images);
			} catch {
				// allocation failed; user can retry
			}
		},
		[sendChat, ensureSessionId],
	);

	// Modes are backend-advertised: the available set + current mode come from
	// the server per active backend (wingman / codex / claude each differ).
	const [modes, setModes] = useState<ModeOption[]>([]);
	const [mode, setMode] = useState<string>("");

	useEffect(() => {
		const q = sessionId ? `?session=${encodeURIComponent(sessionId)}` : "";
		fetch(`/api/mode${q}`)
			.then((r) => r.json())
			.then((data) => {
				setModes(data.modes ?? []);
				setMode(data.current ?? "");
			})
			.catch(() => {});
	}, [sessionId]);

	const selectMode = useCallback(
		async (next: string) => {
			const prev = mode;
			try {
				const sid = await ensureSessionId();
				setMode(next); // optimistic
				const r = await fetch(`/api/mode?session=${encodeURIComponent(sid)}`, {
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify({ mode: next }),
				});
				if (!r.ok) throw new Error(await r.text());
				const data = await r.json();
				setModes(data.modes ?? []);
				setMode(data.current ?? next);
			} catch {
				setMode(prev); // revert on failure
			}
		},
		[ensureSessionId, mode],
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
			<Group
				orientation="horizontal"
				id="wingman-layout"
				defaultLayout={defaultLayout}
				onLayoutChanged={onLayoutChanged}
				className="flex-1 overflow-hidden"
			>
				{/* Left Sidebar */}
				<Panel
					panelRef={leftPanelRef}
					id="sidebar"
					defaultSize="0px"
					collapsible
					collapsedSize="0px"
					minSize="224px"
					groupResizeBehavior="preserve-pixel-size"
					onResize={({ inPixels }) => setSidebarCollapsed(inPixels === 0)}
					className="overflow-hidden"
				>
					<Sidebar
						currentSessionId={sessionId}
						onSessionSelect={handleSessionSelect}
						onNewSession={handleNewSession}
						onSessionDeleted={handleSessionDeleted}
						runningSessionIds={runningSessionIds}
						canCreateNew={canCreateNew}
						subscribe={subscribe}
					/>
				</Panel>
				<ResizeHandle />

				{/* Center Panel */}
				<Panel
					id="center"
					minSize="320px"
					className="flex flex-col overflow-hidden min-w-0 bg-bg"
				>
					<div className="h-10 flex items-stretch bg-bg shrink-0 overflow-x-auto">
						<button
							type="button"
							className="self-center flex items-center justify-center w-8 h-8 ml-1 rounded-md text-fg-dim hover:text-fg-muted hover:bg-bg-hover cursor-pointer transition-colors shrink-0"
							onClick={() => togglePanel(leftPanelRef.current)}
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
							const isDirty = dirtyTabs.has(tab.id);
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
												{isDirty ? (
													<span
														className={`group-hover:hidden w-2 h-2 rounded-full ${active ? "bg-fg-muted" : "bg-fg-dim"}`}
														aria-label="Unsaved changes"
													/>
												) : (
													<Icon
														size={13}
														className={`group-hover:hidden ${active ? "text-fg-muted" : "text-fg-dim"}`}
													/>
												)}
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
						{(usage.inputTokens > 0 || outputTokens > 0) && (
							<div className="flex items-center px-3 text-[11px] text-fg-dim tabular-nums whitespace-nowrap">
								{"↑"}
								{formatTokens(usage.inputTokens)}
								{usage.cachedTokens > 0 && (
									<span className="ml-1">
										({formatTokens(usage.cachedTokens)} cached)
									</span>
								)}
								<span className="ml-2">
									{streamEstimate > 0 ? "↓~" : "↓"}
									{formatTokens(outputTokens)}
								</span>
							</div>
						)}
						<button
							type="button"
							className="self-center flex items-center justify-center w-8 h-8 mr-1 rounded-md text-fg-dim hover:text-fg-muted hover:bg-bg-hover cursor-pointer transition-colors shrink-0"
							onClick={() => togglePanel(rightPanelRef.current)}
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
								entries={entries}
								phase={phase}
								modes={modes}
								mode={mode}
								onSelectMode={selectMode}
								onSend={handleSend}
								onCancel={handleCancel}
								loading={loadingSession}
								loadError={loadError}
								subscribe={subscribe}
								prompt={prompt}
								onPromptReply={handlePromptReply}
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
								key={activeTab.id}
								path={activeTab.path}
								line={activeTab.line}
								subscribe={subscribe}
								onDeleted={() => closeTab(activeTab.id)}
								onDirtyChange={(d) => setTabDirty(activeTab.id, d)}
							/>
						) : null}
					</div>
				</Panel>
				<ResizeHandle />

				{/* Right Panel */}
				<Panel
					panelRef={rightPanelRef}
					id="right"
					defaultSize="288px"
					collapsible
					collapsedSize="0px"
					minSize="288px"
					groupResizeBehavior="preserve-pixel-size"
					onResize={({ inPixels }) => setRightPanelCollapsed(inPixels === 0)}
					className="overflow-hidden"
				>
					<div className="h-full flex flex-col bg-bg">
						<div className="h-10 flex items-stretch shrink-0">
							{showChanges && (
								<RightTabButton
									active={rightTab === "changes"}
									onClick={() => setRequestedRightTab("changes")}
								>
									Changes
								</RightTabButton>
							)}
							<RightTabButton
								active={rightTab === "files"}
								onClick={() => setRequestedRightTab("files")}
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
										<CheckpointsPanel
											sessionId={sessionId}
											subscribe={subscribe}
										/>
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
				</Panel>
			</Group>

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

// estimateStreamingTokens approximates output tokens for the in-flight request
// (~4 chars/token) from the trailing run of streamed reasoning/assistant text.
// The run breaks at the last tool result — mirroring how committed usage jumps
// per request — so it doesn't double-count earlier requests in the same turn.
function estimateStreamingTokens(entries: ChatEntry[]): number {
	let chars = 0;
	for (let i = entries.length - 1; i >= 0; i--) {
		const e = entries[i];
		if (e.type !== "reasoning" && e.type !== "assistant") break;
		chars += e.content.length;
	}
	return Math.floor(chars / 4);
}

function togglePanel(panel: PanelImperativeHandle | null) {
	if (!panel) return;
	if (panel.isCollapsed()) panel.expand();
	else panel.collapse();
}

function ResizeHandle() {
	return (
		<Separator className="w-px shrink-0 bg-border-subtle outline-none hover:bg-accent active:bg-accent transition-colors" />
	);
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
