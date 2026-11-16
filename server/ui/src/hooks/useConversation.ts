import { useQuery } from "@tanstack/react-query";
import { useCallback, useLayoutEffect, useRef, useState } from "react";
import { fetchJSON } from "../api/http.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { base64PCM, openConversationAudio } from "./conversationAudio.ts";
import { meterLevel } from "./useVoiceInput.ts";

export function useConversationAvailable(backend: string, catalogue: string) {
	const supported =
		typeof AudioWorkletNode !== "undefined" &&
		!!navigator.mediaDevices?.getUserMedia;
	const { data } = useQuery({
		queryKey: ["server", "conversation", backend, catalogue],
		queryFn: ({ signal }) =>
			fetchJSON<{ available: boolean }>(
				`/api/v2/backends/${encodeURIComponent(backend)}/conversation`,
				{ signal },
			),
		enabled: supported && !!backend,
		staleTime: 60_000,
	});
	return supported && data?.available === true;
}

type Audio = Awaited<ReturnType<typeof openConversationAudio>>;
type Active = {
	abort: AbortController;
	socket?: WebSocket;
	audio?: Audio;
	timeout?: ReturnType<typeof setTimeout>;
	playback?: ReturnType<typeof setTimeout>;
};

export function useConversation({
	backend,
	owner,
	getSession,
	onError,
	onNotice,
}: {
	backend: string;
	owner: string;
	getSession: () => Promise<string>;
	onError: (error: Error) => void;
	onNotice: (message: string) => void;
}) {
	const [phase, setPhase] = useState<
		"idle" | "connecting" | "listening" | "speaking"
	>("idle");
	const [muted, setMuted] = useState(false);
	const level = useRef(0);
	const current = useRef<Active | null>(null);
	const callbacks = useRef({ getSession, onError, onNotice });
	useLayoutEffect(() => {
		callbacks.current = { getSession, onError, onNotice };
	});
	const stop = useCallback(() => {
		const active = current.current;
		current.current = null;
		clearTimeout(active?.timeout);
		clearTimeout(active?.playback);
		active?.abort.abort();
		active?.audio?.close();
		active?.socket?.close();
		setPhase("idle");
		setMuted(false);
	}, []);
	const start = useCallback(async () => {
		if (current.current) return;
		const active: Active = { abort: new AbortController() };
		current.current = active;
		setPhase("connecting");
		level.current = 0;
		const client = workspaceClient();
		let ready = false;
		const fail = (error: unknown) => {
			if (current.current !== active) return;
			stop();
			callbacks.current.onError(
				error instanceof Error ? error : new Error(String(error)),
			);
		};
		try {
			const audio = await openConversationAudio((pcm, amplitude) => {
				if (
					!ready ||
					current.current !== active ||
					active.socket?.readyState !== WebSocket.OPEN
				)
					return;
				if (active.socket.bufferedAmount > 1 << 20) {
					fail(new Error("Voice connection cannot keep up."));
					return;
				}
				active.socket.send(
					JSON.stringify({ type: "audio", audio: base64PCM(pcm) }),
				);
				level.current = meterLevel(amplitude);
			}, active.abort.signal);
			if (current.current !== active) {
				audio.close();
				return;
			}
			active.audio = audio;
			// Permission prompts are user-controlled and may stay open for
			// longer than the network connection timeout.
			active.timeout = setTimeout(
				() => fail(new Error("Voice conversation did not connect.")),
				30_000,
			);
			const session = await callbacks.current.getSession();
			if (current.current !== active) return;
			const url = new URL(
				client.http.workspaceURL(
					`/api/v2/chats/${encodeURIComponent(session)}/conversation`,
				),
				location.href,
			);
			url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
			const socket = new WebSocket(url);
			active.socket = socket;
			socket.onmessage = (message) => {
				if (current.current !== active) return;
				try {
					const event = JSON.parse(message.data);
					switch (event.type) {
						case "ready":
							ready = true;
							clearTimeout(active.timeout);
							setPhase("listening");
							break;
						case "audio":
							if (!ready) return;
							clearTimeout(active.playback);
							audio.play(event.audio, event.item_id, event.content_index ?? 0);
							setPhase("speaking");
							break;
						case "audio_done":
							active.playback = setTimeout(() => {
								if (current.current === active) setPhase("listening");
							}, audio.remainingMS());
							break;
						case "interrupted": {
							clearTimeout(active.playback);
							for (const position of audio.interrupt())
								socket.send(JSON.stringify({ type: "truncate", ...position }));
							setPhase("listening");
							break;
						}
						case "error":
							fail(new Error(event.message));
							break;
						case "notice":
							callbacks.current.onNotice(event.message);
							break;
					}
				} catch (error) {
					fail(error);
				}
			};
			socket.onerror = () =>
				fail(new Error("Could not connect the voice conversation."));
			socket.onclose = () => {
				if (current.current === active)
					fail(new Error("Voice conversation disconnected."));
			};
		} catch (error) {
			fail(error);
		}
	}, [stop]);
	const toggleMuted = () =>
		setMuted((value) => {
			current.current?.audio?.mute(!value);
			return !value;
		});
	useLayoutEffect(() => stop, [backend, owner, stop]);
	return { phase, muted, level, start, stop, toggleMuted };
}
