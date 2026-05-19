import { useCallback, useEffect, useRef, useState } from "react";
import type {
	ClientMessage,
	ConversationMessage,
	Phase,
	ServerMessage,
} from "../types/protocol";

export interface ChatEntry {
	id: string;
	type: "user" | "assistant" | "tool" | "reasoning" | "error";
	content: string;
	images?: string[];
	toolName?: string;
	toolArgs?: string;
	toolHint?: string;
	toolResult?: string;
	toolId?: string;
	reasoningId?: string;
}

export function messagesToEntries(messages: ConversationMessage[]): ChatEntry[] {
	const entries: ChatEntry[] = [];
	for (const m of messages) {
		// Collect images in this message so we can attach them to the user
		// entry alongside its text (or stand them up as an image-only entry).
		const isUser = m.role === "user";
		const msgImages: string[] = isUser
			? m.content.flatMap((c) => (c.image?.data ? [c.image.data] : []))
			: [];
		let imagesAttached = msgImages.length === 0;

		for (const c of m.content) {
			if (c.text) {
				const entry: ChatEntry = {
					id: crypto.randomUUID(),
					type: isUser ? "user" : "assistant",
					content: c.text,
				};
				if (!imagesAttached) {
					entry.images = msgImages;
					imagesAttached = true;
				}
				entries.push(entry);
			}
			if (c.reasoning?.summary) {
				entries.push({
					id: crypto.randomUUID(),
					type: "reasoning",
					content: c.reasoning.summary,
					reasoningId: c.reasoning.id,
				});
			}
			if (c.tool_call) {
				// Restore the call: this matches the entry the live stream
				// would have produced (tool_call event), with the same toolId
				// the result will later match against.
				entries.push({
					id: c.tool_call.id || crypto.randomUUID(),
					type: "tool",
					content: "",
					toolId: c.tool_call.id,
					toolName: c.tool_call.name,
					toolArgs: c.tool_call.args,
					toolHint: c.tool_call.hint,
				});
			}
			if (c.tool_result) {
				// Attach to the call entry by id; fall back to a standalone
				// entry if the call wasn't in the message list (defensive).
				const existing =
					c.tool_result.id !== undefined
						? entries.findLast(
								(e) => e.type === "tool" && e.toolId === c.tool_result?.id,
							)
						: undefined;
				if (existing) {
					existing.toolResult = c.tool_result.content;
				} else {
					entries.push({
						id: c.tool_result.id || crypto.randomUUID(),
						type: "tool",
						content: "",
						toolId: c.tool_result.id,
						toolName: c.tool_result.name,
						toolArgs: c.tool_result.args,
						toolResult: c.tool_result.content,
					});
				}
			}
		}
		// User message with image(s) but no text — render an image-only entry.
		if (!imagesAttached) {
			entries.push({
				id: crypto.randomUUID(),
				type: "user",
				content: "",
				images: msgImages,
			});
		}
	}
	return entries;
}

interface Usage {
	inputTokens: number;
	cachedTokens: number;
	outputTokens: number;
}

// SessionState is everything the UI needs to render one session view. The
// hook stores a map keyed by session id; concurrent sends in different
// sessions update their own slots without cross-contamination.
export interface SessionState {
	id: string;
	entries: ChatEntry[];
	phase: Phase;
	usage: Usage;
}

const EMPTY_USAGE: Usage = { inputTokens: 0, cachedTokens: 0, outputTokens: 0 };

function emptySession(id: string): SessionState {
	return { id, entries: [], phase: "idle", usage: EMPTY_USAGE };
}

// Per-session streaming refs (which assistant entry is currently growing,
// which reasoning block, etc.). Kept off React state so streaming deltas
// don't re-render every consumer of useWebSocket.
interface StreamRefs {
	streamingId: string;
	streamingContent: string;
	reasoningEntryId: string;
	reasoningId: string;
	reasoningContent: string;
}

function emptyStreamRefs(): StreamRefs {
	return {
		streamingId: "",
		streamingContent: "",
		reasoningEntryId: "",
		reasoningId: "",
		reasoningContent: "",
	};
}

export function useWebSocket() {
	const wsRef = useRef<WebSocket | null>(null);
	const [connected, setConnected] = useState(false);
	const [sessions, setSessions] = useState<Record<string, SessionState>>({});

	const streamRefs = useRef<Record<string, StreamRefs>>({});

	// Panels (FileTree, DiffsPanel, …) subscribe to raw frames so they can
	// refetch on workspace-level events without going through React state.
	const subscribersRef = useRef<Set<(msg: ServerMessage) => void>>(new Set());

	const subscribe = useCallback((handler: (msg: ServerMessage) => void) => {
		subscribersRef.current.add(handler);
		return () => {
			subscribersRef.current.delete(handler);
		};
	}, []);

	const nextId = () => crypto.randomUUID();

	const getStream = (id: string): StreamRefs => {
		let s = streamRefs.current[id];
		if (!s) {
			s = emptyStreamRefs();
			streamRefs.current[id] = s;
		}
		return s;
	};

	// updateSession applies a transformation to one session's slot, creating
	// it if missing. The reducer pattern lets handlers compose ("set phase
	// AND append entry") without separate setter calls.
	const updateSession = useCallback(
		(id: string, fn: (s: SessionState) => SessionState) => {
			setSessions((prev) => {
				const existing = prev[id];
				const base = existing ?? emptySession(id);
				const updated = fn(base);
				if (existing && updated === existing) return prev;
				return { ...prev, [id]: updated };
			});
		},
		[],
	);

	const finalizeStreaming = (id: string) => {
		const s = getStream(id);
		s.streamingId = "";
		s.streamingContent = "";
	};

	const finalizeReasoning = (id: string) => {
		const s = getStream(id);
		s.reasoningEntryId = "";
		s.reasoningId = "";
		s.reasoningContent = "";
	};

	const handleMessageRef = useRef<(msg: ServerMessage) => void>(() => {});

	const handleMessage = (msg: ServerMessage) => {
		for (const sub of subscribersRef.current) {
			sub(msg);
		}

		// Workspace-level events (no session) propagate through subscribers
		// only — they don't touch per-session state.
		const sid = "session" in msg ? msg.session : undefined;
		if (!sid) return;

		switch (msg.type) {
			case "session_state": {
				// Authoritative snapshot for this session on WS connect.
				streamRefs.current[sid] = emptyStreamRefs();
				setSessions((prev) => ({
					...prev,
					[sid]: {
						id: sid,
						entries: messagesToEntries(msg.messages ?? []),
						phase: msg.phase ?? "idle",
						usage: {
							inputTokens: msg.input_tokens ?? 0,
							cachedTokens: msg.cached_tokens ?? 0,
							outputTokens: msg.output_tokens ?? 0,
						},
					},
				}));
				break;
			}

			case "text_delta": {
				finalizeReasoning(sid);
				const s = getStream(sid);
				let entryId = s.streamingId;
				if (!entryId) {
					entryId = nextId();
					s.streamingId = entryId;
				}
				s.streamingContent += msg.text;
				const content = s.streamingContent;
				updateSession(sid, (sess) => {
					const existing = sess.entries.find((e) => e.id === entryId);
					if (!existing) {
						return {
							...sess,
							entries: [
								...sess.entries,
								{ id: entryId, type: "assistant", content },
							],
						};
					}
					return {
						...sess,
						entries: sess.entries.map((e) =>
							e.id === entryId ? { ...e, content } : e,
						),
					};
				});
				break;
			}

			case "reasoning_delta": {
				finalizeStreaming(sid);
				const s = getStream(sid);
				// New reasoning block (different upstream id) — start fresh.
				if (s.reasoningEntryId && s.reasoningId !== msg.id) {
					finalizeReasoning(sid);
				}
				let entryId = s.reasoningEntryId;
				if (!entryId) {
					entryId = nextId();
					s.reasoningEntryId = entryId;
					s.reasoningId = msg.id;
				}
				s.reasoningContent += msg.text;
				const content = s.reasoningContent;
				const reasoningId = msg.id;
				updateSession(sid, (sess) => {
					const existing = sess.entries.find((e) => e.id === entryId);
					if (!existing) {
						return {
							...sess,
							entries: [
								...sess.entries,
								{ id: entryId, type: "reasoning", content, reasoningId },
							],
						};
					}
					return {
						...sess,
						entries: sess.entries.map((e) =>
							e.id === entryId ? { ...e, content } : e,
						),
					};
				});
				break;
			}

			case "tool_call": {
				finalizeStreaming(sid);
				finalizeReasoning(sid);
				const entryId = msg.id || nextId();
				updateSession(sid, (sess) => ({
					...sess,
					entries: [
						...sess.entries,
						{
							id: entryId,
							type: "tool",
							content: "",
							toolId: msg.id,
							toolName: msg.name,
							toolArgs: msg.args,
							toolHint: msg.hint,
						},
					],
				}));
				break;
			}

			case "tool_result": {
				updateSession(sid, (sess) => {
					const idx = sess.entries.findLastIndex(
						(e) => e.type === "tool" && e.toolId === msg.id,
					);
					if (idx < 0) return sess;
					const updated = [...sess.entries];
					updated[idx] = { ...updated[idx], toolResult: msg.content };
					return { ...sess, entries: updated };
				});
				break;
			}

			case "phase":
				// phase=idle is the turn-end signal — clear streaming refs so
				// the next turn starts a fresh assistant/reasoning entry.
				if (msg.phase === "idle") {
					finalizeStreaming(sid);
					finalizeReasoning(sid);
				}
				updateSession(sid, (sess) => ({ ...sess, phase: msg.phase }));
				break;

			case "error": {
				finalizeStreaming(sid);
				finalizeReasoning(sid);
				updateSession(sid, (sess) => ({
					...sess,
					entries: [
						...sess.entries,
						{ id: nextId(), type: "error", content: msg.message },
					],
				}));
				break;
			}

			case "usage":
				updateSession(sid, (sess) => ({
					...sess,
					usage: {
						inputTokens: msg.input_tokens,
						cachedTokens: msg.cached_tokens,
						outputTokens: msg.output_tokens,
					},
				}));
				break;
		}
	};

	useEffect(() => {
		handleMessageRef.current = handleMessage;
	});

	const send = useCallback((msg: ClientMessage) => {
		if (wsRef.current?.readyState === WebSocket.OPEN) {
			wsRef.current.send(JSON.stringify(msg));
		}
	}, []);

	const sendChat = useCallback(
		(sessionId: string, text: string, files?: string[], images?: string[]) => {
			const id = nextId();
			updateSession(sessionId, (sess) => ({
				...sess,
				entries: [
					...sess.entries,
					{ id, type: "user", content: text, images },
				],
			}));
			send({ type: "send", session: sessionId, text, files, images });
		},
		[send, updateSession],
	);

	const cancel = useCallback(
		(sessionId: string) => {
			send({ type: "cancel", session: sessionId });
		},
		[send],
	);

	const removeSession = useCallback((sessionId: string) => {
		setSessions((prev) => {
			if (!(sessionId in prev)) return prev;
			const { [sessionId]: _gone, ...rest } = prev;
			return rest;
		});
		delete streamRefs.current[sessionId];
	}, []);

	useEffect(() => {
		let reconnectTimer: ReturnType<typeof setTimeout>;
		let alive = true;
		let cachedURL: string | null = null;

		async function resolveURL(): Promise<string> {
			if (cachedURL) return cachedURL;
			try {
				const r = await fetch("/api/ws");
				if (r.ok) {
					const d = (await r.json()) as { url?: string };
					if (d.url) {
						cachedURL = d.url;
						return cachedURL;
					}
				}
			} catch {
				// fall through to default
			}
			const proto = location.protocol === "https:" ? "wss:" : "ws:";
			cachedURL = `${proto}//${location.host}/ws`;
			return cachedURL;
		}

		async function connect() {
			if (!alive) return;

			const url = await resolveURL();
			if (!alive) return;

			const ws = new WebSocket(url);

			ws.onopen = () => {
				setConnected(true);
				// Trigger panels to refetch in case state changed while
				// disconnected — fsnotify events don't get replayed across
				// reconnects, so this is the only safety net.
				for (const sub of subscribersRef.current) {
					sub({ type: "diffs_changed" });
					sub({ type: "checkpoints_changed" });
					sub({ type: "sessions_changed" });
					sub({ type: "files_changed" });
					sub({ type: "diagnostics_changed" });
					sub({ type: "capabilities_changed" });
				}
			};

			ws.onclose = () => {
				setConnected(false);
				wsRef.current = null;
				if (alive) {
					reconnectTimer = setTimeout(connect, 2000);
				}
			};

			ws.onerror = () => ws.close();

			ws.onmessage = (e) => {
				const msg: ServerMessage = JSON.parse(e.data);
				handleMessageRef.current(msg);
			};

			wsRef.current = ws;
		}

		connect();

		return () => {
			alive = false;
			clearTimeout(reconnectTimer);
			wsRef.current?.close();
		};
	}, []);

	return {
		connected,
		sessions,
		sendChat,
		cancel,
		removeSession,
		subscribe,
	};
}
