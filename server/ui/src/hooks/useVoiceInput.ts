import { useQuery } from "@tanstack/react-query";
import {
	useCallback,
	useEffect,
	useLayoutEffect,
	useRef,
	useState,
} from "react";
import { fetchJSON } from "../api/http.ts";

const SAMPLE_RATE = 16000;
const MIN_SECONDS = 0.5;
const MAX_MS = 2 * 60_000;
const METER_BARS = 4;

export type VoicePhase = "idle" | "recording" | "transcribing";

const voicePath = (backend: string) =>
	`/api/v2/backends/${encodeURIComponent(backend)}/transcription`;

const supported = () =>
	typeof AudioWorkletNode !== "undefined" &&
	!!navigator.mediaDevices?.getUserMedia;

/** The catalogue revision refetches availability once the host discovers models. */
export function useVoiceAvailable(backend: string, catalogue: string) {
	const enabled = supported() && !!backend;
	const { data } = useQuery({
		queryKey: ["server", "transcription", backend, catalogue],
		queryFn: ({ signal }) =>
			fetchJSON<{ available: boolean }>(voicePath(backend), { signal }),
		enabled,
		staleTime: 60_000,
	});
	return enabled && data?.available === true;
}

const captureWorklet = `
class VoiceCapture extends AudioWorkletProcessor {
	process(inputs) {
		const samples = inputs[0]?.[0];
		if (samples) {
			const copy = samples.slice();
			this.port.postMessage(copy, [copy.buffer]);
		}
		return true;
	}
}
registerProcessor("voice-capture", VoiceCapture);
`;

type Recorder = { stop(): Promise<Float32Array>; cancel(): void };

async function startRecorder(
	onLevel: (level: number) => void,
): Promise<Recorder> {
	const stream = await navigator.mediaDevices.getUserMedia({
		audio: {
			channelCount: 1,
			echoCancellation: true,
			noiseSuppression: true,
			autoGainControl: true,
		},
	});
	// Capture at the device rate; Firefox cannot mix rates in one graph.
	const context = new AudioContext();
	const chunks: Float32Array[] = [];
	const close = () => {
		for (const track of stream.getTracks()) track.stop();
		void context.close();
	};
	try {
		const url = URL.createObjectURL(
			new Blob([captureWorklet], { type: "text/javascript" }),
		);
		try {
			await context.audioWorklet.addModule(url);
		} finally {
			URL.revokeObjectURL(url);
		}
		const node = new AudioWorkletNode(context, "voice-capture");
		node.port.onmessage = (event: MessageEvent<Float32Array>) => {
			chunks.push(event.data);
			onLevel(rms(event.data));
		};
		context.createMediaStreamSource(stream).connect(node);
		// The processor outputs silence; the connection keeps the graph pulling audio.
		node.connect(context.destination);
	} catch (error) {
		close();
		throw error;
	}
	return {
		cancel: close,
		async stop() {
			close();
			return resample(chunks, context.sampleRate);
		},
	};
}

function rms(samples: Float32Array) {
	let sum = 0;
	for (const s of samples) sum += s * s;
	return Math.sqrt(sum / Math.max(samples.length, 1));
}

async function resample(chunks: Float32Array[], rate: number) {
	const length = chunks.reduce((n, chunk) => n + chunk.length, 0);
	const input = new Float32Array(length);
	let offset = 0;
	for (const chunk of chunks) {
		input.set(chunk, offset);
		offset += chunk.length;
	}
	if (!length || rate === SAMPLE_RATE) return input;
	const offline = new OfflineAudioContext(
		1,
		Math.ceil((length * SAMPLE_RATE) / rate),
		SAMPLE_RATE,
	);
	const buffer = offline.createBuffer(1, length, rate);
	buffer.copyToChannel(input, 0);
	const source = offline.createBufferSource();
	source.buffer = buffer;
	source.connect(offline.destination);
	source.start();
	return (await offline.startRendering()).getChannelData(0);
}

export function encodeWAV(samples: Float32Array): Blob {
	const view = new DataView(new ArrayBuffer(44 + samples.length * 2));
	const text = (at: number, value: string) => {
		for (let i = 0; i < value.length; i++)
			view.setUint8(at + i, value.charCodeAt(i));
	};
	text(0, "RIFF");
	view.setUint32(4, 36 + samples.length * 2, true);
	text(8, "WAVEfmt ");
	view.setUint32(16, 16, true);
	view.setUint16(20, 1, true);
	view.setUint16(22, 1, true);
	view.setUint32(24, SAMPLE_RATE, true);
	view.setUint32(28, SAMPLE_RATE * 2, true);
	view.setUint16(32, 2, true);
	view.setUint16(34, 16, true);
	text(36, "data");
	view.setUint32(40, samples.length * 2, true);
	samples.forEach((sample, i) => {
		const s = Math.max(-1, Math.min(1, sample));
		view.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
	});
	return new Blob([view.buffer], { type: "audio/wav" });
}

/** Normalizes RMS to 0..1 over -60..-10 dBFS for the level meter. */
export function meterLevel(level: number) {
	const db = 20 * Math.log10(Math.max(level, 1e-6));
	return Math.min(Math.max((db + 60) / 50, 0), 1);
}

function microphoneError(error: unknown) {
	if (error instanceof DOMException && error.name === "NotAllowedError")
		return new Error(
			"Allow microphone access for this page to use voice input.",
		);
	if (error instanceof DOMException && error.name === "NotFoundError")
		return new Error("No microphone found.");
	return error instanceof Error ? error : new Error(String(error));
}

export function useVoiceInput({
	backend,
	onText,
	onError,
}: {
	backend: string;
	onText: (text: string) => void;
	onError: (error: Error) => void;
}) {
	const [phase, setPhase] = useState<VoicePhase>("idle");
	const [levels, setLevels] = useState<number[]>(() =>
		Array(METER_BARS).fill(0),
	);
	const recorder = useRef<Promise<Recorder> | null>(null);
	const request = useRef<AbortController | null>(null);
	const level = useRef(0);
	const callbacks = useRef({ onText, onError });
	useLayoutEffect(() => {
		callbacks.current = { onText, onError };
	});

	const cancel = useCallback(() => {
		recorder.current?.then(
			(r) => r.cancel(),
			() => {},
		);
		recorder.current = null;
		request.current?.abort();
		request.current = null;
		setPhase("idle");
	}, []);

	const stop = useCallback(async () => {
		const pending = recorder.current;
		recorder.current = null;
		if (!pending) return;
		const controller = new AbortController();
		request.current = controller;
		try {
			const samples = await (await pending).stop();
			if (controller.signal.aborted) return;
			if (samples.length < SAMPLE_RATE * MIN_SECONDS) {
				setPhase("idle");
				return;
			}
			if (!samples.some((s) => s !== 0))
				throw new Error("No audio detected from microphone.");
			setPhase("transcribing");
			const { text } = await fetchJSON<{ text: string }>(voicePath(backend), {
				method: "POST",
				headers: { "Content-Type": "audio/wav" },
				body: encodeWAV(samples),
				signal: controller.signal,
			});
			if (!controller.signal.aborted && text) callbacks.current.onText(text);
		} catch (error) {
			if (!controller.signal.aborted)
				callbacks.current.onError(microphoneError(error));
		} finally {
			if (request.current === controller) {
				request.current = null;
				setPhase("idle");
			}
		}
	}, [backend]);

	const start = useCallback(() => {
		if (recorder.current || request.current) return;
		level.current = 0;
		const pending = startRecorder((value) => {
			level.current = value;
		});
		recorder.current = pending;
		setPhase("recording");
		pending.catch((error) => {
			if (recorder.current !== pending) return;
			recorder.current = null;
			setPhase("idle");
			callbacks.current.onError(microphoneError(error));
		});
	}, []);

	useEffect(() => {
		if (phase !== "recording") return;
		const meter = setInterval(
			() => setLevels((prev) => [...prev.slice(1), meterLevel(level.current)]),
			100,
		);
		const limit = setTimeout(() => void stop(), MAX_MS);
		return () => {
			clearInterval(meter);
			clearTimeout(limit);
		};
	}, [phase, stop]);

	// Escape cancels before it can reach the composer and stop the running turn.
	useEffect(() => {
		if (phase === "idle") return;
		const onKey = (event: KeyboardEvent) => {
			if (event.key !== "Escape") return;
			event.preventDefault();
			event.stopPropagation();
			cancel();
		};
		window.addEventListener("keydown", onKey, true);
		return () => window.removeEventListener("keydown", onKey, true);
	}, [phase, cancel]);

	// Cancel before a backend change can send audio or deliver a stale result.
	useLayoutEffect(() => cancel, [backend, cancel]);

	return { phase, levels, start, stop, cancel };
}
