import assert from "node:assert/strict";
import test from "node:test";
import { insertVoiceTranscript } from "../src/api/voice.ts";

test("inserts a transcript at the saved composer selection", () => {
	assert.deepEqual(insertVoiceTranscript("beforeafter", 6, 6, " spoken "), {
		text: "before spoken after",
		caret: 14,
	});
	assert.deepEqual(insertVoiceTranscript("replace this", 0, 7, "keep"), {
		text: "keep this",
		caret: 4,
	});
});

test("preserves surrounding whitespace when inserting voice text", () => {
	assert.deepEqual(insertVoiceTranscript("before  after", 7, 7, "spoken"), {
		text: "before spoken after",
		caret: 13,
	});
});
