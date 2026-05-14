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
		for (const c of m.content) {
			if (c.text) {
				entries.push({
					id: crypto.randomUUID(),
					type: m.role === "user" ? "user" : "assistant",
					content: c.text,
				});
			}
			if (c.reasoning?.summary) {
				entries.push({
					id: crypto.randomUUID(),
					type: "reasoning",
					content: c.reasoning.summary,
					reasoningId: c.reasoning.id,
				});
			}
			if (c.tool_result) {
				entries.push({
					id: crypto.randomUUID(),
					type: "tool",
					content: "",
					toolName: c.tool_result.name,
					toolArgs: c.tool_result.args,
					toolResult: c.tool_result.content,
				});
			}
		}
	}
	return entries;
}

interface Usage {
	inputTokens: number;
	cachedTokens: number;
	outputTokens: number;
}

// SessionState bundles everything the UI needs to render one session view.
// The hook stores a map keyed by session id; concurrent sends in different
// sessions update their own slots without ever cross-contaminating.
export interface SessionState {
	id: string;
	entries: ChatEntry[];
	phase: Phase;
	usage: Usage;
	prompt: { type: "prompt" | "ask"; question: string } | null;
}

function emptySession(id: string): SessionState {
	return {
		id,
		entries: [],
		phase: "idle",
		usage: { inputTokens: 0, cachedTokens: 0, outputTokens: 0 },
		prompt: null,
	};
}

// Per-session streaming refs (which assistant entry is currently growing,
// which reasoning block, etc.). Kept off the React state tree so streaming
// deltas don't re-render every consumer of useWebSocket.
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

	const updateSession = useCallback(
		(id: string, fn: (s: SessionState) => SessionState) => {
			setSessions((prev) => {
				const existing = prev[id] ?? emptySession(id);
				const updated = fn(existing);
				if (updated === existing) return prev;
				return { ...prev, [id]: updated };
			});
		},
		[],
	);

	const finalizeStreaming = (id: string) => {
		const s = getStream(id);
		if (s.streamingId && s.streamingContent) {
			const entryId = s.streamingId;
			const content = s.streamingContent;
			updateSession(id, (sess) => ({
				...sess,
				entries: sess.entries.map((e) =>
					e.id === entryId ? { ...e, content } : e,
				),
			}));
		}
		s.streamingId = "";
		s.streamingContent = "";
	};

	const finalizeReasoning = (id: string) => {
		const s = getStream(id);
		if (s.reasoningEntryId && s.reasoningContent) {
			const entryId = s.reasoningEntryId;
			const content = s.reasoningContent;
			updateSession(id, (sess) => ({
				...sess,
				entries: sess.entries.map((e) =>
					e.id === entryId ? { ...e, content } : e,
				),
			}));
		}
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
		// only — they don't affect per-session state.
		const sid = "session" in msg ? msg.session : undefined;
		if (!sid) return;

		switch (msg.type) {
			case "session": {
				// Server announces a session: hydrate its slot if absent.
				updateSession(sid, (sess) => sess);
				break;
			}

			case "messages": {
				updateSession(sid, (sess) => ({
					...sess,
					entries: messagesToEntries(msg.messages),
				}));
				break;
			}

			case "text_delta": {
				finalizeReasoning(sid);
				const s = getStream(sid);
				if (!s.streamingId) {
					const id = nextId();
					s.streamingId = id;
					s.streamingContent = "";
					updateSession(sid, (sess) => ({
						...sess,
						entries: [
							...sess.entries,
							{ id, type: "assistant", content: "" },
						],
					}));
				}
				s.streamingContent += msg.text;
				const entryId = s.streamingId;
				const content = s.streamingContent;
				updateSession(sid, (sess) => ({
					...sess,
					entries: sess.entries.map((e) =>
						e.id === entryId ? { ...e, content } : e,
					),
				}));
				break;
			}

			case "reasoning_delta": {
				finalizeStreaming(sid);
				const s = getStream(sid);
				if (s.reasoningEntryId && s.reasoningId !== msg.id) {
					finalizeReasoning(sid);
				}
				if (!s.reasoningEntryId) {
					const entryId = nextId();
					s.reasoningEntryId = entryId;
					s.reasoningId = msg.id;
					s.reasoningContent = "";
					updateSession(sid, (sess) => ({
						...sess,
						entries: [
							...sess.entries,
							{
								id: entryId,
								type: "reasoning",
								content: "",
								reasoningId: msg.id,
							},
						],
					}));
				}
				s.reasoningContent += msg.text;
				const entryId = s.reasoningEntryId;
				const content = s.reasoningContent;
				updateSession(sid, (sess) => ({
					...sess,
					entries: sess.entries.map((e) =>
						e.id === entryId ? { ...e, content } : e,
					),
				}));
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
				updateSession(sid, (sess) => ({ ...sess, phase: msg.phase }));
				break;

			case "prompt":
				updateSession(sid, (sess) => ({
					...sess,
					prompt: { type: "prompt", question: msg.question },
				}));
				break;

			case "ask":
				updateSession(sid, (sess) => ({
					...sess,
					prompt: { type: "ask", question: msg.question },
				}));
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

			case "done":
				finalizeStreaming(sid);
				finalizeReasoning(sid);
				break;

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
		(sessionId: string, text: string, files?: string[]) => {
			const id = nextId();
			updateSession(sessionId, (sess) => ({
				...sess,
				entries: [...sess.entries, { id, type: "user", content: text }],
			}));
			send({ type: "send", session: sessionId, text, files });
		},
		[send, updateSession],
	);

	const cancel = useCallback(
		(sessionId: string) => {
			send({ type: "cancel", session: sessionId });
		},
		[send],
	);

	const respondPrompt = useCallback(
		(sessionId: string, approved: boolean) => {
			send({ type: "prompt_response", session: sessionId, approved });
			updateSession(sessionId, (sess) => ({ ...sess, prompt: null }));
		},
		[send, updateSession],
	);

	const respondAsk = useCallback(
		(sessionId: string, answer: string) => {
			send({ type: "ask_response", session: sessionId, answer });
			updateSession(sessionId, (sess) => ({ ...sess, prompt: null }));
		},
		[send, updateSession],
	);

	// Imperative helpers for handlers that mutate session state (load,
	// new, delete).
	const setSessionEntries = useCallback(
		(sessionId: string, entries: ChatEntry[]) => {
			updateSession(sessionId, (sess) => ({ ...sess, entries }));
		},
		[updateSession],
	);

	const setSessionUsage = useCallback(
		(sessionId: string, usage: Usage) => {
			updateSession(sessionId, (sess) => ({ ...sess, usage }));
		},
		[updateSession],
	);

	const removeSession = useCallback((sessionId: string) => {
		setSessions((prev) => {
			if (!(sessionId in prev)) return prev;
			const { [sessionId]: _gone, ...rest } = prev;
			return rest;
		});
		delete streamRefs.current[sessionId];
	}, []);

	const ensureSession = useCallback(
		(sessionId: string) => {
			updateSession(sessionId, (sess) => sess);
		},
		[updateSession],
	);

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
		respondPrompt,
		respondAsk,
		setSessionEntries,
		setSessionUsage,
		ensureSession,
		removeSession,
		subscribe,
	};
}
