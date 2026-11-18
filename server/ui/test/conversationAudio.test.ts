import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { openConversationAudio } from "../src/hooks/conversationAudio.ts";

async function audioFixture(t: TestContext) {
	const context = {
		currentTime: 0,
		state: "running",
		destination: {},
		audioWorklet: { async addModule() {} },
		async resume() {},
		async close() {
			this.state = "closed";
		},
		createMediaStreamSource: () => ({ connect() {} }),
		createBuffer: (_channels: number, length: number, rate: number) => ({
			duration: length / rate,
			getChannelData: () => new Float32Array(length),
		}),
		createBufferSource: () => ({ connect() {}, start() {}, stop() {} }),
	};
	const track = { enabled: true, stop() {} };
	const stream = { getTracks: () => [track], getAudioTracks: () => [track] };
	for (const [key, value] of Object.entries({
		AudioContext: function () {
			return context;
		},
		AudioWorkletNode: function () {
			return { port: {}, connect() {} };
		},
		navigator: { mediaDevices: { getUserMedia: async () => stream } },
	})) {
		const previous = Object.getOwnPropertyDescriptor(globalThis, key);
		Object.defineProperty(globalThis, key, { value, configurable: true });
		t.after(() => {
			if (previous) Object.defineProperty(globalThis, key, previous);
			else Reflect.deleteProperty(globalThis, key);
		});
	}
	const audio = await openConversationAudio(
		() => {},
		new AbortController().signal,
	);
	t.after(audio.close);
	return { audio, context };
}

const second = Buffer.alloc(24000 * 2).toString("base64");

test("interruption counts heard audio across delivery gaps", async (t) => {
	const { audio, context } = await audioFixture(t);
	audio.play(second, "reply", 0);
	context.currentTime = 2; // The first chunk finished before the next arrived.
	audio.play(second, "reply", 0);
	context.currentTime = 2.5;
	assert.deepEqual(audio.interrupt(), [
		{ item_id: "reply", content_index: 0, played_ms: 1500 },
	]);
});

test("interruption during an underrun still truncates the current content", async (t) => {
	const { audio, context } = await audioFixture(t);
	audio.play(second, "reply", 0);
	context.currentTime = 2;
	assert.deepEqual(audio.interrupt(), [
		{ item_id: "reply", content_index: 0, played_ms: 1000 },
	]);
});

test("interruption distinguishes queued content parts and clears playback", async (t) => {
	const { audio, context } = await audioFixture(t);
	audio.play(second, "reply", 0);
	audio.play(second, "reply", 1);
	audio.play(second, "next", 0);
	context.currentTime = 0.5;
	assert.deepEqual(audio.interrupt(), [
		{ item_id: "reply", content_index: 0, played_ms: 500 },
		{ item_id: "reply", content_index: 1, played_ms: 0 },
		{ item_id: "next", content_index: 0, played_ms: 0 },
	]);
	assert.equal(audio.remainingMS(), 0);
	assert.deepEqual(audio.interrupt(), []);
});
