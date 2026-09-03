import { fetchJSON } from "./http.ts";

export interface VoiceTranscription {
	text: string;
	model: string;
}

function audioExtension(type: string): string {
	const mime = type.split(";", 1)[0].toLowerCase();
	switch (mime) {
		case "audio/flac":
			return "flac";
		case "audio/mpeg":
		case "audio/mp3":
			return "mp3";
		case "audio/mp4":
		case "audio/x-m4a":
		case "video/mp4":
			return "m4a";
		case "audio/ogg":
			return "ogg";
		case "audio/wav":
		case "audio/x-wav":
			return "wav";
		default:
			return "webm";
	}
}

export async function transcribeVoice(
	audio: Blob,
	signal?: AbortSignal,
): Promise<VoiceTranscription> {
	const form = new FormData();
	form.append("audio", audio, `voice.${audioExtension(audio.type)}`);
	return fetchJSON<VoiceTranscription>("/api/voice/transcriptions", {
		method: "POST",
		body: form,
		signal,
	});
}

export function insertVoiceTranscript(
	draft: string,
	start: number,
	end: number,
	transcript: string,
): { text: string; caret: number } {
	const spoken = transcript.trim();
	if (spoken === "")
		return { text: draft, caret: Math.min(start, draft.length) };

	const from = Math.max(0, Math.min(start, draft.length));
	const to = Math.max(from, Math.min(end, draft.length));
	const before = draft.slice(0, from);
	const after = draft.slice(to);
	const leading = before !== "" && !/\s$/.test(before) ? " " : "";
	const trailing = after !== "" && !/^\s/.test(after) ? " " : "";
	const insertion = leading + spoken + trailing;
	return {
		text: before + insertion + after,
		caret: before.length + insertion.length,
	};
}
