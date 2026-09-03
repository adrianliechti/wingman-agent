import { useCallback, useEffect, useRef, useState } from "react";
import { transcribeVoice } from "../api/voice.ts";

export type VoiceInputState =
	| "idle"
	| "requesting"
	| "recording"
	| "transcribing";

const MAX_RECORDING_MS = 2 * 60 * 1000;

function preferredMimeType(): string | undefined {
	if (typeof MediaRecorder === "undefined") return undefined;
	for (const type of [
		"audio/webm;codecs=opus",
		"audio/mp4",
		"audio/ogg;codecs=opus",
		"audio/webm",
	]) {
		if (MediaRecorder.isTypeSupported(type)) return type;
	}
	return undefined;
}

function microphoneError(error: unknown): string {
	if (error instanceof DOMException) {
		if (error.name === "NotAllowedError") return "Microphone access was denied";
		if (error.name === "NotFoundError") return "No microphone was found";
	}
	if (error instanceof Error && error.message) return error.message;
	return "Voice input failed";
}

export function useVoiceInput(
	enabled: boolean,
	onTranscript: (text: string) => void,
	onError: (message: string) => void,
) {
	const [state, setState] = useState<VoiceInputState>("idle");
	const recorderRef = useRef<MediaRecorder | null>(null);
	const streamRef = useRef<MediaStream | null>(null);
	const chunksRef = useRef<Blob[]>([]);
	const timeoutRef = useRef<number | null>(null);
	const requestRef = useRef<AbortController | null>(null);
	const disposedRef = useRef(false);
	const transcriptRef = useRef(onTranscript);
	const errorRef = useRef(onError);

	useEffect(() => {
		transcriptRef.current = onTranscript;
		errorRef.current = onError;
	}, [onError, onTranscript]);

	const supported =
		typeof navigator !== "undefined" &&
		!!navigator.mediaDevices?.getUserMedia &&
		typeof MediaRecorder !== "undefined";

	const clearTimer = useCallback(() => {
		if (timeoutRef.current !== null) {
			window.clearTimeout(timeoutRef.current);
			timeoutRef.current = null;
		}
	}, []);

	const releaseStream = useCallback(() => {
		streamRef.current?.getTracks().forEach((track) => track.stop());
		streamRef.current = null;
	}, []);

	const start = useCallback(async () => {
		if (!enabled || !supported || recorderRef.current) return;
		setState("requesting");
		try {
			const stream = await navigator.mediaDevices.getUserMedia({
				audio: {
					channelCount: 1,
					echoCancellation: true,
					noiseSuppression: true,
				},
			});
			if (disposedRef.current) {
				stream.getTracks().forEach((track) => track.stop());
				return;
			}
			if (!enabled) {
				stream.getTracks().forEach((track) => track.stop());
				setState("idle");
				return;
			}

			streamRef.current = stream;
			chunksRef.current = [];
			const mimeType = preferredMimeType();
			const recorder = new MediaRecorder(
				stream,
				mimeType ? { mimeType } : undefined,
			);
			recorderRef.current = recorder;
			recorder.ondataavailable = (event) => {
				if (event.data.size > 0) chunksRef.current.push(event.data);
			};
			recorder.onerror = () => {
				clearTimer();
				releaseStream();
				recorder.onstop = null;
				recorderRef.current = null;
				setState("idle");
				errorRef.current("The browser could not record microphone audio");
			};
			recorder.onstop = () => {
				clearTimer();
				releaseStream();
				recorderRef.current = null;
				const chunks = chunksRef.current;
				chunksRef.current = [];
				if (disposedRef.current) return;
				if (chunks.length === 0) {
					setState("idle");
					errorRef.current("No microphone audio was recorded");
					return;
				}

				setState("transcribing");
				const audio = new Blob(chunks, {
					type: recorder.mimeType || mimeType || "audio/webm",
				});
				const request = new AbortController();
				requestRef.current = request;
				void transcribeVoice(audio, request.signal)
					.then((result) => {
						if (!disposedRef.current) {
							if (result.text.trim() !== "") {
								transcriptRef.current(result.text);
							} else {
								errorRef.current("No speech was detected");
							}
						}
					})
					.catch((error: unknown) => {
						if (!disposedRef.current && !request.signal.aborted) {
							errorRef.current(microphoneError(error));
						}
					})
					.finally(() => {
						if (requestRef.current === request) requestRef.current = null;
						if (!disposedRef.current) setState("idle");
					});
			};
			recorder.start(250);
			setState("recording");
			timeoutRef.current = window.setTimeout(() => {
				if (recorder.state === "recording") recorder.stop();
			}, MAX_RECORDING_MS);
		} catch (error) {
			recorderRef.current = null;
			clearTimer();
			releaseStream();
			if (!disposedRef.current) {
				setState("idle");
				errorRef.current(microphoneError(error));
			}
		}
	}, [clearTimer, enabled, releaseStream, supported]);

	const stop = useCallback(() => {
		const recorder = recorderRef.current;
		if (recorder?.state === "recording") {
			setState("transcribing");
			recorder.stop();
		}
	}, []);

	const toggle = useCallback(() => {
		if (recorderRef.current?.state === "recording") {
			stop();
			return;
		}
		void start();
	}, [start, stop]);

	useEffect(() => {
		disposedRef.current = false;
		return () => {
			disposedRef.current = true;
			clearTimer();
			requestRef.current?.abort();
			requestRef.current = null;
			const recorder = recorderRef.current;
			if (recorder && recorder.state !== "inactive") {
				recorder.onstop = null;
				recorder.stop();
			}
			recorderRef.current = null;
			releaseStream();
		};
	}, [clearTimer, releaseStream]);

	return { state, supported, toggle };
}
