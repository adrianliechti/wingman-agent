import {
	ArrowUp,
	ChevronDown,
	ChevronRight,
	Loader2,
	LoaderCircle,
	Paperclip,
	Plus,
	Square,
	X,
} from "lucide-react";
import {
	useCallback,
	useEffect,
	useLayoutEffect,
	memo,
	useMemo,
	useRef,
	useState,
} from "react";
import { useColorScheme } from "../hooks/useColorScheme";
import type { ChatEntry, PendingPrompt } from "../hooks/useWebSocket";
import { sampleSpinnerVerb } from "../spinnerVerbs";
import type { Phase } from "../types/protocol";
import { FilePicker } from "./FilePicker";
import { MarkdownContent } from "./MarkdownContent";
import { ModelPicker } from "./ModelPicker";
import { ModePicker, type ModeOption } from "./ModePicker";
import { SkillPicker } from "./SkillPicker";

interface Props {
	sessionId?: string;
	entries: ChatEntry[];
	phase: Phase;
	modes: ModeOption[];
	mode: string;
	onSelectMode: (next: string) => void;
	onSend: (text: string, files?: string[], images?: string[]) => void;
	onCancel: () => void;
	loading?: boolean;
	loadError?: string | null;
	subscribe?: (
		handler: (msg: import("../types/protocol").ServerMessage) => void,
	) => () => void;
	prompt?: PendingPrompt | null;
	onPromptReply?: (reply: { text?: string; approved?: boolean }) => void;
}

interface PendingImage {
	id: string;
	dataUrl: string;
	name?: string;
}

function readImageAsDataUrl(file: File): Promise<string> {
	return new Promise((resolve, reject) => {
		const reader = new FileReader();
		reader.onload = () => resolve(reader.result as string);
		reader.onerror = () => reject(reader.error);
		reader.readAsDataURL(file);
	});
}

const MAX_EDGE = 2048;
const JPEG_QUALITY = 0.9;
const PASSTHROUGH_MAX_BYTES = 512 * 1024;

async function processImage(file: File): Promise<string> {
	if (file.type === "image/gif") return readImageAsDataUrl(file);

	let bitmap: ImageBitmap;
	try {
		bitmap = await createImageBitmap(file);
	} catch {
		return readImageAsDataUrl(file);
	}

	try {
		const longest = Math.max(bitmap.width, bitmap.height);
		if (longest <= MAX_EDGE && file.size <= PASSTHROUGH_MAX_BYTES) {
			return readImageAsDataUrl(file);
		}
		const scale = Math.min(1, MAX_EDGE / longest);
		const w = Math.max(1, Math.round(bitmap.width * scale));
		const h = Math.max(1, Math.round(bitmap.height * scale));
		const canvas = document.createElement("canvas");
		canvas.width = w;
		canvas.height = h;
		const ctx = canvas.getContext("2d");
		if (!ctx) return readImageAsDataUrl(file);
		ctx.drawImage(bitmap, 0, 0, w, h);
		return canvas.toDataURL("image/jpeg", JPEG_QUALITY);
	} finally {
		bitmap.close();
	}
}

const PIN_TOP_GAP = 16;

export function ChatPanel({
	sessionId,
	entries,
	phase,
	modes,
	mode,
	onSelectMode,
	onSend,
	onCancel,
	loading,
	loadError,
	subscribe,
	prompt,
	onPromptReply,
}: Props) {
	const scheme = useColorScheme();
	const [input, setInput] = useState("");
	const [files, setFiles] = useState<string[]>([]);
	const [images, setImages] = useState<PendingImage[]>([]);
	const [showPicker, setShowPicker] = useState(false);
	const containerRef = useRef<HTMLDivElement>(null);
	const contentRef = useRef<HTMLDivElement>(null);
	const spacerRef = useRef<HTMLDivElement>(null);
	const textareaRef = useRef<HTMLTextAreaElement>(null);
	const imageInputRef = useRef<HTMLInputElement>(null);
	const turns = useMemo(() => buildTurns(entries), [entries]);
	const latestTurnsRef = useRef<Turn[]>([]);
	useLayoutEffect(() => {
		latestTurnsRef.current = turns;
	}, [turns]);

	const submitPendingRef = useRef(false);
	const pinRef = useRef<{ id: string; top: number } | null>(null);
	const userScrolledRef = useRef(false);
	const programmaticUntilRef = useRef(0);
	const restoredRef = useRef(false);

	const writeScrollTop = useCallback((el: HTMLElement, top: number) => {
		programmaticUntilRef.current = performance.now() + 100;
		el.scrollTop = top;
	}, []);

	const pendingAnchorRef = useRef<{ id: string; viewportTop: number } | null>(
		null,
	);

	const captureAnchorForTurns = useCallback((sourceTurns: Turn[]) => {
		const c = containerRef.current;
		const content = contentRef.current;
		if (!c || !content) return;
		const stable = new Set<string>();
		for (const t of sourceTurns) {
			if (t.user) stable.add(t.user.id);
			if (t.final) stable.add(t.final.id);
		}
		const cRect = c.getBoundingClientRect();
		let visible: { id: string; viewportTop: number } | null = null;
		let below: { id: string; viewportTop: number } | null = null;
		let above: { id: string; viewportTop: number } | null = null;
		const els = content.querySelectorAll<HTMLElement>("[data-entry-id]");
		for (const el of els) {
			const id = el.dataset.entryId;
			if (!id || !stable.has(id)) continue;
			const rect = el.getBoundingClientRect();
			const viewportTop = rect.top - cRect.top;
			const viewportBottom = rect.bottom - cRect.top;
			if (viewportBottom >= 0 && viewportTop <= cRect.height) {
				visible = { id, viewportTop };
				continue;
			}
			if (viewportTop > cRect.height) {
				below = { id, viewportTop };
				break;
			}
			above = { id, viewportTop };
		}
		pendingAnchorRef.current = visible ?? below ?? above;
	}, []);

	const applyPendingAnchor = useCallback(() => {
		const c = containerRef.current;
		const content = contentRef.current;
		if (!c || !content) return;
		const a = pendingAnchorRef.current;
		if (!a) return;
		pendingAnchorRef.current = null;
		const el = findEntryElement(content, a.id);
		if (!el) return;
		const cRect = c.getBoundingClientRect();
		const newTop = el.getBoundingClientRect().top - cRect.top;
		const delta = newTop - a.viewportTop;
		if (Math.abs(delta) > 0.5) {
			writeScrollTop(c, c.scrollTop + delta);
		}
	}, [writeScrollTop]);

	const isActive = phase !== "idle";

	useEffect(() => {
		if (!isActive) textareaRef.current?.focus();
	}, [isActive]);

	const prevPhaseRef = useRef(phase);
	/* eslint-disable react-hooks/refs -- This is intentionally a pre-commit
	   DOM snapshot for the phase-driven auto-collapse. Function components do
	   not have a getSnapshotBeforeUpdate hook, and a layout effect would run
	   after the working entries have already collapsed. */
	if (prevPhaseRef.current !== "idle" && phase === "idle") {
		if (userScrolledRef.current) captureAnchorForTurns(turns);
	}
	prevPhaseRef.current = phase;

	const skillMatch = input.match(/^\/(\S*)$/);
	const showSkills = !!skillMatch;
	const skillQuery = skillMatch ? skillMatch[1] : "";

	useLayoutEffect(() => {
		if (restoredRef.current || entries.length === 0) return;
		restoredRef.current = true;
		if (submitPendingRef.current) return;
		const el = containerRef.current;
		if (el) writeScrollTop(el, el.scrollHeight);
	}, [entries, writeScrollTop]);

	useLayoutEffect(() => {
		const container = containerRef.current;
		const content = contentRef.current;
		const spacer = spacerRef.current;
		if (!container || !content || !spacer) return;

		if (submitPendingRef.current) {
			const last = entries[entries.length - 1];
			if (last?.type !== "user") return;
			const userEl = findEntryElement(content, last.id);
			if (!userEl) return;

			submitPendingRef.current = false;
			userScrolledRef.current = false;

			spacer.style.height = `${container.clientHeight}px`;
			const cRect = container.getBoundingClientRect();
			const uRect = userEl.getBoundingClientRect();
			const top = Math.max(
				0,
				uRect.top - cRect.top + container.scrollTop - PIN_TOP_GAP,
			);
			pinRef.current = { id: last.id, top };
			writeScrollTop(container, top);
			return;
		}

		const pin = pinRef.current;
		if (!pin) return;

		if (phase === "idle") {
			pinRef.current = null;
			const belowUser = container.scrollHeight - pin.top - spacer.offsetHeight;
			const minForPin = Math.max(0, container.clientHeight - belowUser);
			const minForUser = Math.max(
				0,
				container.scrollTop +
					container.clientHeight -
					(container.scrollHeight - spacer.offsetHeight),
			);
			spacer.style.height = `${Math.max(minForPin, minForUser)}px`;
			return;
		}

		if (userScrolledRef.current) return;
		if (Math.abs(container.scrollTop - pin.top) > 2) {
			writeScrollTop(container, pin.top);
		}
	}, [entries, phase, writeScrollTop]);

	useEffect(() => {
		if (phase === "idle") return;
		const container = containerRef.current;
		const content = contentRef.current;
		const spacer = spacerRef.current;
		if (!container || !content || !spacer) return;

		const onResize = () => {
			const pin = pinRef.current;
			if (!pin || userScrolledRef.current) return;
			const userEl = findEntryElement(content, pin.id);
			if (!userEl) return;

			const cRect = container.getBoundingClientRect();
			const uRect = userEl.getBoundingClientRect();
			const top = Math.max(
				0,
				uRect.top - cRect.top + container.scrollTop - PIN_TOP_GAP,
			);
			pin.top = top;
			spacer.style.height = `${container.clientHeight}px`;
			writeScrollTop(container, top);
		};

		window.addEventListener("resize", onResize);
		return () => window.removeEventListener("resize", onResize);
	}, [phase, writeScrollTop]);

	useEffect(() => {
		const container = containerRef.current;
		if (!container) return;
		const onScroll = () => {
			if (performance.now() < programmaticUntilRef.current) return;
			userScrolledRef.current = true;
		};
		container.addEventListener("scroll", onScroll, { passive: true });
		return () => container.removeEventListener("scroll", onScroll);
	}, []);

	const handleSubmit = useCallback(() => {
		const text = input.trim();
		if (!text && images.length === 0) return;
		submitPendingRef.current = true;
		onSend(
			text,
			files.length > 0 ? files : undefined,
			images.length > 0 ? images.map((i) => i.dataUrl) : undefined,
		);
		setInput("");
		setFiles([]);
		setImages([]);
		textareaRef.current?.focus();
	}, [input, onSend, files, images]);

	const handleKeyDown = useCallback(
		(e: React.KeyboardEvent) => {
			if (
				showSkills &&
				(e.key === "Enter" ||
					e.key === "Tab" ||
					e.key === "ArrowDown" ||
					e.key === "ArrowUp" ||
					e.key === "Escape")
			) {
				return;
			}
			if (e.key === "Enter" && !e.shiftKey) {
				e.preventDefault();
				handleSubmit();
			}
			if (e.key === "Escape" && isActive) {
				onCancel();
			}
		},
		[handleSubmit, isActive, onCancel, showSkills],
	);

	const addFile = useCallback((path: string) => {
		setFiles((prev) => (prev.includes(path) ? prev : [...prev, path]));
		setShowPicker(false);
		textareaRef.current?.focus();
	}, []);

	const removeFile = useCallback((path: string) => {
		setFiles((prev) => prev.filter((p) => p !== path));
	}, []);

	const addImageFiles = useCallback(async (fileList: FileList | File[]) => {
		const next: PendingImage[] = [];
		for (const f of Array.from(fileList)) {
			if (!f.type.startsWith("image/")) continue;
			try {
				const dataUrl = await processImage(f);
				next.push({ id: crypto.randomUUID(), dataUrl, name: f.name });
			} catch {}
		}
		if (next.length > 0) setImages((prev) => [...prev, ...next]);
	}, []);

	const removeImage = useCallback((id: string) => {
		setImages((prev) => prev.filter((i) => i.id !== id));
	}, []);

	const handlePaste = useCallback(
		(e: React.ClipboardEvent<HTMLTextAreaElement>) => {
			const items = e.clipboardData?.items;
			if (!items) return;
			const pasted: File[] = [];
			for (const item of items) {
				if (item.kind !== "file") continue;
				if (!item.type.startsWith("image/")) continue;
				const f = item.getAsFile();
				if (f) pasted.push(f);
			}
			if (pasted.length === 0) return;
			e.preventDefault();
			void addImageFiles(pasted);
		},
		[addImageFiles],
	);

	const selectSkill = useCallback(
		(s: { name: string; arguments?: string[] }) => {
			const hasArgs = !!s.arguments && s.arguments.length > 0;
			if (hasArgs) {
				setInput(`/${s.name} `);
				textareaRef.current?.focus();
				return;
			}
			submitPendingRef.current = true;
			onSend(
				`/${s.name}`,
				files.length > 0 ? files : undefined,
				images.length > 0 ? images.map((i) => i.dataUrl) : undefined,
			);
			setInput("");
			setFiles([]);
			setImages([]);
		},
		[onSend, files, images],
	);

	return (
		<div className="h-full relative overflow-hidden bg-bg">
			<div
				className="h-full overflow-y-auto pb-24 [overflow-anchor:none]"
				ref={containerRef}
			>
				{loading && entries.length === 0 ? (
					<div className="h-full flex items-center justify-center">
						<Loader2 size={16} className="text-fg-dim animate-spin" />
					</div>
				) : loadError ? (
					<div className="h-full flex items-center justify-center">
						<div className="max-w-sm px-4 text-center text-[13px] text-danger break-words">
							{loadError}
						</div>
					</div>
				) : entries.length === 0 && phase === "idle" ? (
					<div className="h-full flex items-center justify-center">
						<div className="flex flex-col items-center text-center max-w-sm">
							<img
								src={scheme === "light" ? "/icon_light.svg" : "/icon_dark.svg"}
								alt="Wingman"
								className="w-20 h-20 mb-4 opacity-80"
							/>
							<div className="text-[13px] text-fg-dim leading-relaxed">
								Ask me to write code, fix bugs, explore files, or run commands.
							</div>
						</div>
					</div>
				) : (
					<div className="px-4 py-4" ref={contentRef}>
						{turns.map((turn, idx) => {
							const isLastTurn = idx === turns.length - 1;
							const isActive = isLastTurn && phase !== "idle";
							return (
								<TurnView
									key={turn.key}
									turn={turn}
									isActive={isActive}
									phase={phase}
									applyPendingAnchor={applyPendingAnchor}
								/>
							);
						})}
					</div>
				)}
				<div ref={spacerRef} aria-hidden style={{ height: 0 }} />
			</div>

			<div className="absolute bottom-0 left-0 right-0">
				<div className="h-6 bg-gradient-to-t from-bg to-transparent pointer-events-none" />
				<div className="bg-bg px-4 pb-3">
					{prompt && onPromptReply ? (
						<PromptBar prompt={prompt} onReply={onPromptReply} />
					) : (
						<div className="relative rounded-lg border border-border-subtle bg-bg-surface/60 hover:border-border focus-within:border-border transition-colors">
							{showSkills && (
								<SkillPicker
									query={skillQuery}
									onSelect={selectSkill}
									onClose={() => {}}
								/>
							)}
							{files.length > 0 && (
								<div className="flex flex-wrap gap-1 px-2.5 pt-2">
									{files.map((p) => {
										const name = p.split("/").pop() || p;
										return (
											<span
												key={p}
												className="group flex items-center gap-1 px-1.5 py-0.5 rounded bg-bg-active text-[11px] text-fg-muted font-mono"
												title={p}
											>
												<span className="truncate max-w-[180px]">{name}</span>
												<button
													type="button"
													className="text-fg-dim hover:text-fg cursor-pointer"
													onClick={() => removeFile(p)}
													aria-label="Remove file"
												>
													<X size={10} />
												</button>
											</span>
										);
									})}
								</div>
							)}

							{images.length > 0 && (
								<div className="flex flex-wrap gap-1.5 px-2.5 pt-2">
									{images.map((img) => (
										<span
											key={img.id}
											className="group relative inline-flex items-center rounded overflow-hidden bg-bg-active"
											title={img.name || "image"}
										>
											<img
												src={img.dataUrl}
												alt={img.name || "image"}
												className="block w-12 h-12 object-cover"
											/>
											<button
												type="button"
												className="absolute top-0.5 right-0.5 w-4 h-4 flex items-center justify-center rounded-full bg-bg/80 text-fg-dim hover:text-fg cursor-pointer"
												onClick={() => removeImage(img.id)}
												aria-label="Remove image"
											>
												<X size={10} />
											</button>
										</span>
									))}
								</div>
							)}

							<div className="px-3 pt-2">
								<textarea
									ref={textareaRef}
									// biome-ignore lint/a11y/noAutofocus: chat input is the primary control
									autoFocus
									className="w-full bg-transparent text-fg text-[12px] font-mono resize-none outline-none leading-[1.7] placeholder:text-fg-dim max-h-[40vh] overflow-y-auto"
									style={{ fieldSizing: "content" } as React.CSSProperties}
									value={input}
									onChange={(e) => setInput(e.target.value)}
									onKeyDown={handleKeyDown}
									onPaste={handlePaste}
									placeholder="Message Wingman…"
									rows={1}
								/>
							</div>

							<div className="flex items-center justify-between px-1.5 pb-1.5 pt-1 gap-1">
								<div className="flex items-center gap-0 min-w-0">
									<div className="relative flex items-center">
										<button
											type="button"
											className="w-7 h-7 flex items-center justify-center rounded text-fg-dim hover:text-fg hover:bg-bg-hover cursor-pointer transition-colors"
											onClick={() => setShowPicker((s) => !s)}
											title="Add file context"
										>
											<Plus size={14} />
										</button>
										{showPicker && (
											<FilePicker
												onSelect={addFile}
												onClose={() => setShowPicker(false)}
											/>
										)}
									</div>
									<input
										ref={imageInputRef}
										type="file"
										accept="image/*"
										multiple
										className="hidden"
										onChange={(e) => {
											if (e.target.files) void addImageFiles(e.target.files);
											e.target.value = "";
										}}
									/>
									<ModePicker
										modes={modes}
										current={mode}
										onSelect={onSelectMode}
									/>
									<ModelPicker sessionId={sessionId} subscribe={subscribe} />
								</div>

								<div className="flex items-center gap-0">
									<button
										type="button"
										className="w-7 h-7 flex items-center justify-center rounded text-fg-dim hover:text-fg hover:bg-bg-hover cursor-pointer transition-colors"
										onClick={() => imageInputRef.current?.click()}
										title="Attach image"
									>
										<Paperclip size={14} />
									</button>
									{(() => {
										const hasInput = input.trim() !== "" || images.length > 0;
										const mode: "send" | "stop" | "disabled" = hasInput
											? "send"
											: isActive
												? "stop"
												: "disabled";
										return (
											<button
												type="button"
												className={`group w-7 h-7 flex items-center justify-center rounded cursor-pointer transition-colors ${
													mode === "disabled"
														? "text-fg-dim opacity-40 cursor-not-allowed"
														: "text-fg-muted hover:text-fg hover:bg-bg-hover"
												}`}
												onClick={mode === "stop" ? onCancel : handleSubmit}
												disabled={mode === "disabled"}
												title={
													mode === "stop"
														? "Stop (Esc)"
														: mode === "send" && isActive
															? "Queue (Enter)"
															: "Send (Enter)"
												}
											>
												{mode === "stop" ? (
													<>
														<LoaderCircle
															size={14}
															className="animate-spin group-hover:hidden"
														/>
														<Square
															size={10}
															fill="currentColor"
															className="hidden group-hover:block"
														/>
													</>
												) : (
													<ArrowUp size={14} />
												)}
											</button>
										);
									})()}
								</div>
							</div>
						</div>
					)}
				</div>
			</div>
		</div>
	);
}

function PromptBar({
	prompt,
	onReply,
}: {
	prompt: PendingPrompt;
	onReply: (reply: { text?: string; approved?: boolean }) => void;
}) {
	const [text, setText] = useState("");
	const inputRef = useRef<HTMLTextAreaElement>(null);

	useEffect(() => {
		inputRef.current?.focus();
	}, []);

	const submitAsk = useCallback(() => {
		const value = text.trim();
		if (!value) return;
		onReply({ text: value });
	}, [text, onReply]);

	const handleKeyDown = useCallback(
		(e: React.KeyboardEvent) => {
			if (prompt.kind !== "ask") return;
			if (e.key === "Enter" && !e.shiftKey) {
				e.preventDefault();
				submitAsk();
			}
		},
		[prompt.kind, submitAsk],
	);

	return (
		<div className="relative rounded-lg border border-warning bg-bg-surface/60">
			<div className="px-3 pt-2 pb-1 flex items-start gap-2">
				<span className="text-warning font-mono text-[12px] leading-[1.7] shrink-0">
					?
				</span>
				<span className="text-fg font-mono text-[12px] leading-[1.7] whitespace-pre-wrap break-words">
					{prompt.message}
				</span>
			</div>
			{prompt.kind === "ask" ? (
				<>
					<div className="px-3">
						<textarea
							ref={inputRef}
							// biome-ignore lint/a11y/noAutofocus: prompt is the primary control while open
							autoFocus
							className="w-full bg-transparent text-fg text-[12px] font-mono resize-none outline-none leading-[1.7] placeholder:text-fg-dim max-h-[40vh] overflow-y-auto"
							style={{ fieldSizing: "content" } as React.CSSProperties}
							value={text}
							onChange={(e) => setText(e.target.value)}
							onKeyDown={handleKeyDown}
							placeholder="Type your answer and press Enter…"
							rows={1}
						/>
					</div>
					<div className="flex items-center justify-end px-1.5 pb-1.5 pt-1">
						<button
							type="button"
							className="px-3 h-7 flex items-center justify-center rounded text-[11px] text-fg-muted hover:text-fg hover:bg-bg-hover cursor-pointer transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
							onClick={submitAsk}
							disabled={text.trim() === ""}
							title="Submit (Enter)"
						>
							Send
						</button>
					</div>
				</>
			) : (
				<div className="flex items-center justify-end gap-1 px-1.5 pb-1.5 pt-1">
					<button
						type="button"
						className="px-3 h-7 flex items-center justify-center rounded text-[11px] text-fg-muted hover:text-fg hover:bg-bg-hover cursor-pointer transition-colors"
						onClick={() => onReply({ approved: false })}
					>
						Deny
					</button>
					<button
						type="button"
						className="px-3 h-7 flex items-center justify-center rounded text-[11px] text-bg bg-success hover:opacity-90 cursor-pointer transition-opacity"
						onClick={() => onReply({ approved: true })}
					>
						Approve
					</button>
				</div>
			)}
		</div>
	);
}

const EntryView = memo(function EntryView({
	entry,
	isStreaming,
}: {
	entry: ChatEntry;
	isStreaming: boolean;
}) {
	const [hovered, setHovered] = useState(false);

	if (entry.type === "error") {
		return (
			<div
				data-entry-id={entry.id}
				className="mb-4 border-l-2 border-danger pl-3"
			>
				<div className="text-[13px] leading-relaxed text-danger break-words">
					{entry.content}
				</div>
			</div>
		);
	}

	if (entry.type === "reasoning") {
		return <ReasoningView entry={entry} isStreaming={isStreaming} />;
	}

	const isUser = entry.type === "user";

	return (
		<div
			data-entry-id={entry.id}
			className="mb-4"
			onMouseEnter={() => setHovered(true)}
			onMouseLeave={() => setHovered(false)}
		>
			<div
				className={`border-l-2 ${isUser ? "border-success" : "border-purple"} pl-3 text-[12px] leading-[1.7] break-words min-w-0 font-mono`}
			>
				{isUser ? (
					<>
						{entry.content && (
							<span className="whitespace-pre-wrap text-fg">
								{entry.content}
							</span>
						)}
						{entry.images && entry.images.length > 0 && (
							<div
								className={`flex flex-wrap gap-1.5 ${entry.content ? "mt-2" : ""}`}
							>
								{entry.images.map((src, i) => (
									<a
										key={`${entry.id}-img-${i}`}
										href={src}
										target="_blank"
										rel="noreferrer"
										className="block rounded overflow-hidden bg-bg-active"
									>
										<img
											src={src}
											alt="attachment"
											className="block max-h-40 max-w-[240px] object-contain"
										/>
									</a>
								))}
							</div>
						)}
					</>
				) : (
					<MarkdownContent text={entry.content} />
				)}
			</div>
			{!isStreaming && entry.content && (
				<div className="pl-3 h-4 mt-0.5 flex items-center">
					{hovered && <CopyTextButton text={entry.content} label="Copy" />}
				</div>
			)}
		</div>
	);
});

interface Turn {
	key: string;
	user: ChatEntry | null;
	working: ChatEntry[];
	final: ChatEntry | null;
}

function buildTurns(entries: ChatEntry[]): Turn[] {
	const turns: Turn[] = [];
	let counter = 0;

	for (const e of entries) {
		if (e.type === "user" || turns.length === 0) {
			turns.push({
				key: e.type === "user" ? e.id : `turn-${counter++}`,
				user: e.type === "user" ? e : null,
				working: [],
				final: null,
			});
			if (e.type === "user") continue;
		}
		const t = turns[turns.length - 1];
		if (t.final) {
			t.working.push(t.final);
			t.final = null;
		}
		if (e.type === "assistant" || e.type === "error") {
			t.final = e;
		} else {
			t.working.push(e);
		}
	}

	return turns;
}

function findEntryElement(root: HTMLElement, id: string): HTMLElement | null {
	for (const el of root.querySelectorAll<HTMLElement>("[data-entry-id]")) {
		if (el.dataset.entryId === id) return el;
	}
	return null;
}

const TurnView = memo(function TurnView({
	turn,
	isActive,
	phase,
	applyPendingAnchor,
}: {
	turn: Turn;
	isActive: boolean;
	phase: Phase;
	applyPendingAnchor: () => void;
}) {
	const canCollapse =
		!isActive && turn.final !== null && turn.working.length > 0;
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
			{turn.user && (
				<EntryView entry={turn.user} isStreaming={false} />
			)}
			{turn.working.length > 0 &&
				(expanded ? (
					<>
						<WorkingExpanded
							entries={turn.working}
							isActive={isActive}
							phase={phase}
							canCollapse={canCollapse}
							onCollapse={() => setExpanded(false)}
						/>
						{isActive &&
							!(phase === "streaming" && turn.final?.type === "assistant") &&
							turn.working[turn.working.length - 1]?.type !== "reasoning" && (
								<PhaseIndicator />
							)}
					</>
				) : (
					<WorkingHeader
						entries={turn.working}
						expanded={false}
						onToggle={() => setExpanded(true)}
						className="mb-4"
					/>
				))}
			{turn.final && (
				<EntryView
					entry={turn.final}
					isStreaming={
						isActive && phase === "streaming" && turn.final.type === "assistant"
					}
				/>
			)}
			{isActive && turn.working.length === 0 && !turn.final && (
				<PhaseIndicator />
			)}
		</>
	);
}, areTurnPropsEqual);

function areTurnPropsEqual(
	prev: {
		turn: Turn;
		isActive: boolean;
		phase: Phase;
	},
	next: {
		turn: Turn;
		isActive: boolean;
		phase: Phase;
	},
) {
	if (!sameTurnEntries(prev.turn, next.turn)) return false;
	if (prev.isActive !== next.isActive) return false;
	if (!prev.isActive && !next.isActive) return true;
	return (
		prev.phase === next.phase &&
		prev.turn.working.length === next.turn.working.length
	);
}

function sameTurnEntries(a: Turn, b: Turn) {
	if (a.key !== b.key || a.user !== b.user || a.final !== b.final) return false;
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
}: {
	entries: ChatEntry[];
	isActive: boolean;
	phase: Phase;
	canCollapse: boolean;
	onCollapse: () => void;
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
			<EntryView key={entry.id} entry={entry} isStreaming={isStreaming} />,
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

function workingSummaryText(entries: ChatEntry[]): string {
	const tools = entries.filter((e) => e.type === "tool").length;
	const thoughts = entries.filter((e) => e.type === "reasoning").length;
	const parts: string[] = [];
	if (thoughts) parts.push(`${thoughts} thought${thoughts === 1 ? "" : "s"}`);
	if (tools) parts.push(`${tools} tool${tools === 1 ? "" : "s"}`);
	return parts.length > 0 ? parts.join(", ") : "Worked";
}

// Disclosure header for the working steps. Same row toggles both ways:
// down-chevron when expanded (steps shown below), right-chevron when collapsed.
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
			className={`flex items-center gap-2 py-0.5 cursor-pointer text-[11px] text-fg-dim hover:text-fg transition-colors ${className}`}
		>
			{expanded ? (
				<ChevronDown size={12} className="shrink-0" />
			) : (
				<ChevronRight size={12} className="shrink-0" />
			)}
			<span className="font-mono">{workingSummaryText(entries)}</span>
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
		<div className="mb-4 pl-3">
			<div className="flex items-baseline gap-1.5 text-[12px] text-fg-dim font-mono italic">
				<span>{verb}</span>
				<span className="inline-flex gap-[3px]">
					<span
						className="w-[3px] h-[3px] rounded-full bg-fg-dim animate-pulse"
						style={{ animationDelay: "0ms", animationDuration: "1.2s" }}
					/>
					<span
						className="w-[3px] h-[3px] rounded-full bg-fg-dim animate-pulse"
						style={{ animationDelay: "200ms", animationDuration: "1.2s" }}
					/>
					<span
						className="w-[3px] h-[3px] rounded-full bg-fg-dim animate-pulse"
						style={{ animationDelay: "400ms", animationDuration: "1.2s" }}
					/>
				</span>
				<span className="not-italic text-fg-dim/70">
					{elapsed}s · esc to interrupt
				</span>
			</div>
		</div>
	);
}

const ToolGroupView = memo(function ToolGroupView({
	entries,
	isTrailing,
	phase,
}: {
	entries: ChatEntry[];
	isTrailing: boolean;
	phase: Phase;
}) {
	return (
		<div className="mb-4">
			{entries.map((entry) => {
				const running = isTrailing && phase !== "idle" && !entry.toolResult;
				return <ToolRow key={entry.id} entry={entry} running={running} />;
			})}
		</div>
	);
});

const ToolRow = memo(function ToolRow({
	entry,
	running,
}: {
	entry: ChatEntry;
	running: boolean;
}) {
	const [expanded, setExpanded] = useState(false);
	const [hovered, setHovered] = useState(false);
	const displayHint = entry.toolHint ? truncate(entry.toolHint, 80) : "";

	return (
		<div
			data-entry-id={entry.id}
			onMouseEnter={() => setHovered(true)}
			onMouseLeave={() => setHovered(false)}
		>
			<div className="border-l-2 border-purple pl-3">
				<div
					className="flex items-center gap-2 py-0.5 cursor-pointer text-[12px] transition-colors"
					onClick={() => setExpanded(!expanded)}
				>
					<span className="text-fg-dim shrink-0 flex items-center">
						{running ? (
							<LoaderCircle size={11} className="animate-spin" />
						) : expanded ? (
							<ChevronDown size={12} />
						) : (
							<ChevronRight size={12} />
						)}
					</span>
					<span className="text-purple font-mono text-[11px] shrink-0">
						{entry.toolName}
					</span>
					{displayHint && (
						<span className="text-fg-dim font-mono text-[11px] overflow-hidden text-ellipsis whitespace-nowrap flex-1">
							{displayHint}
						</span>
					)}
				</div>
				{expanded && (
					<div className="mt-1 px-3 py-2 text-[11px] whitespace-pre-wrap break-all text-fg-dim bg-bg-surface rounded-md font-mono leading-relaxed">
						{truncate(entry.toolResult || "(no output)", 2000)}
					</div>
				)}
			</div>
			{expanded && entry.toolResult && (
				<div className="pl-3 h-4 mt-0.5 flex items-center">
					{hovered && <CopyTextButton text={entry.toolResult} label="Copy" />}
				</div>
			)}
		</div>
	);
});

const ReasoningView = memo(function ReasoningView({
	entry,
	isStreaming,
}: {
	entry: ChatEntry;
	isStreaming: boolean;
}) {
	const summary = entry.content || "";
	if (!summary) return null;

	return (
		<div
			data-entry-id={entry.id}
			className="mb-4 border-l-2 border-purple pl-3"
		>
			<div className="text-[11px] whitespace-pre-wrap break-words text-fg-dim font-mono leading-relaxed italic">
				{summary}
				{isStreaming && (
					<span className="inline-block w-[3px] h-[10px] bg-fg-dim/40 align-text-bottom ml-0.5 animate-[pulse_1.4s_ease-in-out_infinite]" />
				)}
			</div>
		</div>
	);
});

function truncate(text: string, max: number): string {
	return text.length <= max ? text : `${text.substring(0, max)}...`;
}

function CopyTextButton({
	text,
	label = "Copy",
	className = "",
}: {
	text: string;
	label?: string;
	className?: string;
}) {
	const [copied, setCopied] = useState(false);
	const timer = useRef<number | null>(null);

	useEffect(
		() => () => {
			if (timer.current) window.clearTimeout(timer.current);
		},
		[],
	);

	const handleClick = useCallback(
		(e: React.MouseEvent<HTMLButtonElement>) => {
			e.stopPropagation();
			navigator.clipboard
				.writeText(text)
				.then(() => {
					setCopied(true);
					if (timer.current) window.clearTimeout(timer.current);
					timer.current = window.setTimeout(() => setCopied(false), 1200);
				})
				.catch(() => {});
		},
		[text],
	);

	return (
		<button
			type="button"
			onClick={handleClick}
			className={`text-[10px] text-fg-dim hover:text-fg transition-colors cursor-pointer ${className}`}
		>
			{copied ? "Copied" : label}
		</button>
	);
}
