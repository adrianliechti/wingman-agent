import { useCallback, useEffect, useRef, useState } from "react";
import type {
	ClientMessage,
	ConversationMessage,
	Phase,
	PromptKind,
	ServerMessage,
} from "../types/protocol";

export interface PendingPrompt {
	id: string;
	kind: PromptKind;
	message: string;
}

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

export function messagesToEntries(
	messages: ConversationMessage[],
): ChatEntry[] {
	const entries: ChatEntry[] = [];
	messages.forEach((m, mi) => {
		const isUser = m.role === "user";
		const msgImages: string[] = isUser
			? m.content.flatMap((c) => (c.image?.data ? [c.image.data] : []))
			: [];
		let imagesAttached = msgImages.length === 0;

		m.content.forEach((c, ci) => {
			if (c.text) {
				const entry: ChatEntry = {
					id: `t-${mi}-${ci}`,
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
					id: `r-${mi}-${ci}`,
					type: "reasoning",
					content: c.reasoning.summary,
					reasoningId: c.reasoning.id,
				});
			}
			if (c.tool_call) {
				entries.push({
					id: c.tool_call.id || `tc-${mi}-${ci}`,
					type: "tool",
					content: "",
					toolId: c.tool_call.id,
					toolName: c.tool_call.name,
					toolArgs: c.tool_call.args,
					toolHint: c.tool_call.hint,
				});
			}
			if (c.tool_result) {
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
						id: c.tool_result.id || `tr-${mi}-${ci}`,
						type: "tool",
						content: "",
						toolId: c.tool_result.id,
						toolName: c.tool_result.name,
						toolArgs: c.tool_result.args,
						toolResult: c.tool_result.content,
					});
				}
			}
		});
		if (!imagesAttached) {
			entries.push({
				id: `img-${mi}`,
				type: "user",
				content: "",
				images: msgImages,
			});
		}
	});
	return entries;
}

interface Usage {
	inputTokens: number;
	cachedTokens: number;
	outputTokens: number;
}

export interface SessionState {
	id: string;
	entries: ChatEntry[];
	phase: Phase;
	usage: Usage;
	prompt: PendingPrompt | null;
}

const EMPTY_USAGE: Usage = { inputTokens: 0, cachedTokens: 0, outputTokens: 0 };

function emptySession(id: string): SessionState {
	return { id, entries: [], phase: "idle", usage: EMPTY_USAGE, prompt: null };
}

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

	const sessionsSnapshotRef = useRef(sessions);
	useEffect(() => {
		sessionsSnapshotRef.current = sessions;
	}, [sessions]);
	const hasSession = useCallback(
		(id: string) => id in sessionsSnapshotRef.current,
		[],
	);

	const streamRefs = useRef<Record<string, StreamRefs>>({});
	const pendingFlushSessions = useRef<Set<string>>(new Set());
	const flushFrameRef = useRef<number | null>(null);

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
				const existing = prev[id];
				const base = existing ?? emptySession(id);
				const updated = fn(base);
				if (existing && updated === existing) return prev;
				return { ...prev, [id]: updated };
			});
		},
		[],
	);

	const flushStreamSession = useCallback(
		(id: string) => {
			pendingFlushSessions.current.delete(id);
			const s = streamRefs.current[id];
			if (!s) return;

			const streaming =
				s.streamingId && s.streamingContent
					? {
							id: s.streamingId,
							content: s.streamingContent,
						}
					: null;
			const reasoning =
				s.reasoningEntryId && s.reasoningContent
					? {
							id: s.reasoningEntryId,
							content: s.reasoningContent,
							reasoningId: s.reasoningId,
						}
					: null;
			if (!streaming && !reasoning) return;

			updateSession(id, (sess) => {
				let entries = sess.entries;
				let changed = false;

				const upsert = (entry: ChatEntry) => {
					const idx = entries.findIndex((e) => e.id === entry.id);
					if (idx < 0) {
						entries = [...entries, entry];
						changed = true;
						return;
					}

					const existing = entries[idx];
					if (
						existing.content === entry.content &&
						existing.reasoningId === entry.reasoningId
					) {
						return;
					}

					const next = entries === sess.entries ? [...entries] : entries;
					next[idx] = { ...existing, ...entry };
					entries = next;
					changed = true;
				};

				if (reasoning) {
					upsert({
						id: reasoning.id,
						type: "reasoning",
						content: reasoning.content,
						reasoningId: reasoning.reasoningId,
					});
				}
				if (streaming) {
					upsert({
						id: streaming.id,
						type: "assistant",
						content: streaming.content,
					});
				}

				return changed ? { ...sess, entries } : sess;
			});
		},
		[updateSession],
	);

	const flushPendingStreams = useCallback(() => {
		flushFrameRef.current = null;
		const ids = Array.from(pendingFlushSessions.current);
		pendingFlushSessions.current.clear();
		for (const id of ids) flushStreamSession(id);
	}, [flushStreamSession]);

	const scheduleStreamFlush = useCallback(
		(id: string) => {
			pendingFlushSessions.current.add(id);
			if (flushFrameRef.current !== null) return;
			flushFrameRef.current = window.requestAnimationFrame(flushPendingStreams);
		},
		[flushPendingStreams],
	);

	const flushActiveStream = useCallback(
		(id: string) => {
			const s = streamRefs.current[id];
			if (!s?.streamingId && !s?.reasoningEntryId) return;
			flushStreamSession(id);
		},
		[flushStreamSession],
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

		const sid = "session" in msg ? msg.session : undefined;
		if (!sid) return;

		switch (msg.type) {
			case "session_state": {
				pendingFlushSessions.current.delete(sid);
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
						prompt: prev[sid]?.prompt ?? null,
					},
				}));
				break;
			}

			case "text_delta": {
				if (streamRefs.current[sid]?.reasoningEntryId) {
					flushStreamSession(sid);
				}
				finalizeReasoning(sid);
				const s = getStream(sid);
				let entryId = s.streamingId;
				if (!entryId) {
					entryId = nextId();
					s.streamingId = entryId;
				}
				s.streamingContent += msg.text;
				scheduleStreamFlush(sid);
				break;
			}

			case "reasoning_delta": {
				if (streamRefs.current[sid]?.streamingId) {
					flushStreamSession(sid);
				}
				finalizeStreaming(sid);
				const s = getStream(sid);
				if (s.reasoningEntryId && s.reasoningId !== msg.id) {
					flushStreamSession(sid);
					finalizeReasoning(sid);
				}
				let entryId = s.reasoningEntryId;
				if (!entryId) {
					entryId = nextId();
					s.reasoningEntryId = entryId;
					s.reasoningId = msg.id;
				}
				s.reasoningContent += msg.text;
				scheduleStreamFlush(sid);
				break;
			}

			case "tool_call": {
				flushActiveStream(sid);
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
				if (msg.phase === "idle") {
					flushActiveStream(sid);
					finalizeStreaming(sid);
					finalizeReasoning(sid);
				}
				updateSession(sid, (sess) => ({ ...sess, phase: msg.phase }));
				break;

			case "error": {
				flushActiveStream(sid);
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
						inputTokens: msg.input_tokens ?? 0,
						cachedTokens: msg.cached_tokens ?? 0,
						outputTokens: msg.output_tokens ?? 0,
					},
				}));
				break;

			case "prompt":
				updateSession(sid, (sess) => ({
					...sess,
					prompt: {
						id: msg.prompt_id,
						kind: msg.prompt_kind,
						message: msg.message,
					},
				}));
				break;

			case "prompt_cancel":
				updateSession(sid, (sess) =>
					sess.prompt?.id === msg.prompt_id ? { ...sess, prompt: null } : sess,
				);
				break;
		}
	};

	useEffect(() => {
		handleMessageRef.current = handleMessage;
	});

	const send = useCallback((msg: ClientMessage): boolean => {
		if (wsRef.current?.readyState !== WebSocket.OPEN) {
			return false;
		}
		wsRef.current.send(JSON.stringify(msg));
		return true;
	}, []);

	const sendChat = useCallback(
		(sessionId: string, text: string, files?: string[], images?: string[]) => {
			flushActiveStream(sessionId);
			const id = nextId();
			updateSession(sessionId, (sess) => ({
				...sess,
				entries: [...sess.entries, { id, type: "user", content: text, images }],
			}));
			send({ type: "send", session: sessionId, text, files, images });
		},
		[flushActiveStream, send, updateSession],
	);

	const cancel = useCallback(
		(sessionId: string) => {
			send({ type: "cancel", session: sessionId });
		},
		[send],
	);

	useEffect(() => {
		const onFocus = () => send({ type: "focus" });
		window.addEventListener("focus", onFocus);
		return () => window.removeEventListener("focus", onFocus);
	}, [send]);

	const respondPrompt = useCallback(
		(
			sessionId: string,
			promptId: string,
			reply: { text?: string; approved?: boolean },
		) => {
			const sent = send({
				type: "prompt_response",
				session: sessionId,
				prompt_id: promptId,
				text: reply.text,
				approved: reply.approved,
			});
			if (!sent) return;
			updateSession(sessionId, (sess) =>
				sess.prompt?.id === promptId ? { ...sess, prompt: null } : sess,
			);
		},
		[send, updateSession],
	);

	const removeSession = useCallback((sessionId: string) => {
		setSessions((prev) => {
			if (!(sessionId in prev)) return prev;
			const rest = { ...prev };
			delete rest[sessionId];
			return rest;
		});
		delete streamRefs.current[sessionId];
		pendingFlushSessions.current.delete(sessionId);
	}, []);

	const clearSessions = useCallback(() => {
		setSessions({});
		streamRefs.current = {};
		pendingFlushSessions.current.clear();
	}, []);

	useEffect(() => {
		return () => {
			if (flushFrameRef.current !== null) {
				window.cancelAnimationFrame(flushFrameRef.current);
			}
		};
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
			} catch {}
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
		hasSession,
		sendChat,
		cancel,
		respondPrompt,
		removeSession,
		clearSessions,
		subscribe,
	};
}
