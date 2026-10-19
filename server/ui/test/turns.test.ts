import assert from "node:assert/strict";
import test from "node:test";
import { buildTurns } from "../src/components/chat/turns.ts";
import type { ChatEntry } from "../src/types/session.ts";

function entry(id: string, type: ChatEntry["type"]): ChatEntry {
	return { id, type, content: id };
}

test("keeps all consecutive answer messages visible after the work", () => {
	const user = entry("question", "user");
	const reasoning = entry("thinking", "reasoning");
	const diagram = entry("diagram", "assistant");
	const explanation = entry("explanation", "assistant");
	const [turn] = buildTurns([user, reasoning, diagram, explanation]);
	assert.equal(turn.user, user);
	assert.deepEqual(turn.working, [reasoning]);
	assert.deepEqual(turn.reply, [diagram, explanation]);
});

test("collapses progress before a tool while preserving every answer part", () => {
	const progress = entry("progress", "assistant");
	const tool = entry("tool", "tool");
	const answer = entry("answer", "assistant");
	const summary = entry("summary", "assistant");
	const [turn] = buildTurns([progress, tool, answer, summary]);
	assert.deepEqual(turn.working, [progress, tool]);
	assert.deepEqual(turn.reply, [answer, summary]);
});

test("keeps unfinished work expanded and separates answers at user messages", () => {
	const first = entry("first", "user");
	const answer = entry("answer", "assistant");
	const second = entry("second", "user");
	const progress = entry("progress", "assistant");
	const reasoning = entry("thinking", "reasoning");
	const turns = buildTurns([first, answer, second, progress, reasoning]);
	assert.equal(turns.length, 2);
	assert.deepEqual(turns[0].reply, [answer]);
	assert.equal(turns[1].user, second);
	assert.deepEqual(turns[1].working, [progress, reasoning]);
	assert.deepEqual(turns[1].reply, []);
});
