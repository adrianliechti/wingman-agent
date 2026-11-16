const RATE = 24000;

// Resample continuously in the worklet so chunk boundaries never drop samples.
const capture = `
class ConversationCapture extends AudioWorkletProcessor {
 constructor() {
  super(); this.ratio = sampleRate / 24000; this.sum = 0; this.weight = 0;
  this.buffer = new Int16Array(2400); this.at = 0;
 }
 process(inputs) {
  const samples = inputs[0]?.[0];
  if (!samples) return true;
  for (const sample of samples) {
   let remaining = 1;
   while (remaining > 1e-8) {
    const take = Math.min(remaining, this.ratio - this.weight);
    this.sum += sample * take; this.weight += take; remaining -= take;
    if (this.weight >= this.ratio - 1e-8) {
     const value = Math.max(-1, Math.min(1, this.sum / this.ratio));
     this.buffer[this.at++] = value < 0 ? value * 32768 : value * 32767;
     this.sum = 0; this.weight = 0;
     if (this.at === this.buffer.length) {
      this.port.postMessage(this.buffer.buffer, [this.buffer.buffer]);
      this.buffer = new Int16Array(2400); this.at = 0;
     }
    }
   }
  }
  return true;
 }
}
registerProcessor("conversation-capture", ConversationCapture);
`;

export function base64PCM(bytes: Uint8Array) {
	let binary = "";
	for (const byte of bytes) binary += String.fromCharCode(byte);
	return btoa(binary);
}

export async function openConversationAudio(
	onAudio: (pcm: Uint8Array, level: number) => void,
	signal: AbortSignal,
) {
	// Create and resume from the user's gesture, before waiting for permissions or networking.
	const context = new AudioContext();
	const sources = new Set<AudioBufferSourceNode>();
	let stream: MediaStream | undefined;
	let next = 0;
	let items: {
		id: string;
		index: number;
		start: number;
		duration: number;
	}[] = [];
	const interrupt = () => {
		const played = items
			.filter((item) => item.start + item.duration > context.currentTime)
			.map((item) => ({
				item_id: item.id,
				content_index: item.index,
				played_ms: Math.floor(
					Math.min(
						item.duration,
						Math.max(0, context.currentTime - item.start),
					) * 1000,
				),
			}));
		for (const source of sources) source.stop();
		sources.clear();
		next = 0;
		items = [];
		return played;
	};
	const close = () => {
		signal.removeEventListener("abort", close);
		interrupt();
		for (const track of stream?.getTracks() ?? []) track.stop();
		if (context.state !== "closed") void context.close().catch(() => {});
	};
	signal.addEventListener("abort", close, { once: true });
	try {
		signal.throwIfAborted();
		await context.resume();
		signal.throwIfAborted();
		stream = await navigator.mediaDevices.getUserMedia({
			audio: {
				channelCount: 1,
				echoCancellation: true,
				noiseSuppression: true,
				autoGainControl: true,
			},
		});
		signal.throwIfAborted();
		const url = URL.createObjectURL(
			new Blob([capture], { type: "text/javascript" }),
		);
		try {
			await context.audioWorklet.addModule(url);
		} finally {
			URL.revokeObjectURL(url);
		}
		signal.throwIfAborted();
		const node = new AudioWorkletNode(context, "conversation-capture");
		node.port.onmessage = (event: MessageEvent<ArrayBuffer>) => {
			const pcm = new Uint8Array(event.data);
			const view = new DataView(event.data);
			let sum = 0;
			for (let i = 0; i < pcm.length; i += 2)
				sum += (view.getInt16(i, true) / 32768) ** 2;
			onAudio(pcm, Math.sqrt(sum / (pcm.length / 2)));
		};
		context.createMediaStreamSource(stream).connect(node);
		node.connect(context.destination);
		return {
			close,
			interrupt,
			mute(muted: boolean) {
				for (const track of stream!.getAudioTracks()) track.enabled = !muted;
			},
			play(audio: string, id: string, index: number) {
				const binary = atob(audio);
				if (!binary.length || binary.length % 2)
					throw new Error("Invalid voice audio received.");
				const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
				const view = new DataView(bytes.buffer);
				const buffer = context.createBuffer(1, bytes.length / 2, RATE);
				const data = buffer.getChannelData(0);
				for (let i = 0; i < data.length; i++)
					data[i] = view.getInt16(i * 2, true) / 32768;
				const start = Math.max(context.currentTime, next);
				// Match the terminal's 16 MiB PCM budget. Audio is generated
				// faster than playback, so ordinary long answers can exceed 30s.
				if (
					(start + buffer.duration - context.currentTime) * RATE * 2 >
					16 << 20
				)
					throw new Error("Voice playback cannot keep up.");
				items = items.filter(
					(item) => item.start + item.duration > context.currentTime,
				);
				let item = items.at(-1);
				if (!item || item.id !== id) {
					item = { id, index, start, duration: 0 };
					items.push(item);
				}
				item.duration += buffer.duration;
				const source = context.createBufferSource();
				source.buffer = buffer;
				source.connect(context.destination);
				source.onended = () => {
					sources.delete(source);
					source.disconnect();
				};
				sources.add(source);
				source.start(start);
				next = start + buffer.duration;
			},
			remainingMS() {
				return Math.max(0, next - context.currentTime) * 1000;
			},
		};
	} catch (error) {
		close();
		throw error;
	}
}
