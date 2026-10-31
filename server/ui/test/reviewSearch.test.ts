import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import {
	streamWorkspaceSearch,
	type WorkspaceSearchFile,
} from "../src/api/search.ts";

const reviewRequest = {
	query: "needle",
	replacement: "",
	regex: false,
	case_sensitive: false,
	whole_word: false,
	include: "",
	exclude: "",
};
const reviewFile = (path: string): WorkspaceSearchFile => ({
	path,
	revision: "r",
	matches: [
		{
			line: 1,
			column: 1,
			end_column: 7,
			before: "« ",
			text: "needle",
			after: " ☃ 🌍",
			replacement: "",
		},
	],
});

/** Serves the body as the given byte chunks; `hold` keeps the stream open. */
function reviewServe(
	t: TestContext,
	chunks: Uint8Array[],
	options: { hold?: boolean; status?: number } = {},
) {
	const state = {
		cancelled: false,
		signal: undefined as AbortSignal | undefined,
	};
	t.mock.method(
		globalThis,
		"fetch",
		async (_input: RequestInfo | URL, init?: RequestInit) => {
			const signal = init?.signal ?? undefined;
			state.signal = signal;
			let index = 0;
			const body = new ReadableStream<Uint8Array>({
				// Like fetch, an aborted request errors its body stream.
				start(controller) {
					signal?.addEventListener(
						"abort",
						() => controller.error(signal.reason),
						{ once: true },
					);
				},
				pull(controller) {
					if (index < chunks.length) controller.enqueue(chunks[index++]);
					else if (!options.hold) controller.close();
					else return new Promise<void>(() => {});
				},
				cancel() {
					state.cancelled = true;
				},
			});
			return new Response(body, { status: options.status ?? 200 });
		},
	);
	return state;
}
function reviewBytes(text: string) {
	return new TextEncoder().encode(text);
}
function reviewSplit(bytes: Uint8Array, size: number) {
	const out: Uint8Array[] = [];
	for (let i = 0; i < bytes.length; i += size)
		out.push(bytes.slice(i, i + size));
	return out;
}

test("events split at every byte, including inside multi-byte characters, are reassembled", async (t) => {
	const files = [reviewFile("a.ts"), reviewFile("dir/ü b.ts")];
	const body = reviewBytes(
		[
			...files.map((file) => JSON.stringify({ type: "file", file })),
			JSON.stringify({ type: "done", files: 2, matches: 2, truncated: true }),
		].join("\n") + "\n",
	);
	for (const size of [1, 2, 3, 7, body.length]) {
		reviewServe(t, reviewSplit(body, size));
		const seen: WorkspaceSearchFile[] = [];
		const summary = await streamWorkspaceSearch(reviewRequest, (file) =>
			seen.push(file),
		);
		assert.deepEqual(seen, files, `chunk size ${size}`);
		assert.deepEqual(summary, { files: 2, matches: 2, truncated: true });
		t.mock.restoreAll();
	}
});

test("CRLF lines, blank lines and a final line without a newline parse", async (t) => {
	reviewServe(t, [
		reviewBytes(
			`\r\n${JSON.stringify({ type: "file", file: reviewFile("a") })}\r\n\n  \n`,
		),
		reviewBytes(
			JSON.stringify({ type: "done", files: 1, matches: 1, truncated: false }),
		),
	]);
	const seen: string[] = [];
	const summary = await streamWorkspaceSearch(reviewRequest, (file) =>
		seen.push(file.path),
	);
	assert.deepEqual(seen, ["a"]);
	assert.equal(summary.truncated, false);
});

test("a stream that ends mid-line or without a summary fails", async (t) => {
	reviewServe(t, [
		reviewBytes(
			`${JSON.stringify({ type: "file", file: reviewFile("a") })}\n{"type":"do`,
		),
	]);
	await assert.rejects(
		streamWorkspaceSearch(reviewRequest, () => {}),
		SyntaxError,
	);
	t.mock.restoreAll();
	reviewServe(t, [
		reviewBytes(`${JSON.stringify({ type: "file", file: reviewFile("a") })}\n`),
	]);
	await assert.rejects(
		streamWorkspaceSearch(reviewRequest, () => {}),
		/without a summary/,
	);
	t.mock.restoreAll();
	reviewServe(t, [
		reviewBytes(`${JSON.stringify({ type: "error", message: "bad regex" })}\n`),
	]);
	await assert.rejects(
		streamWorkspaceSearch(reviewRequest, () => {}),
		/bad regex/,
	);
	t.mock.restoreAll();
	reviewServe(t, [reviewBytes("invalid pattern")], { status: 400 });
	await assert.rejects(
		streamWorkspaceSearch(reviewRequest, () => {}),
		/invalid pattern/,
	);
});

test("aborting mid-line stops delivery and rejects with the abort reason", async (t) => {
	const state = reviewServe(
		t,
		[
			reviewBytes(
				`${JSON.stringify({ type: "file", file: reviewFile("a") })}\n{"type":"file","fi`,
			),
		],
		{ hold: true },
	);
	const controller = new AbortController();
	const seen: string[] = [];
	const search = streamWorkspaceSearch(
		reviewRequest,
		(file) => {
			seen.push(file.path);
			controller.abort(new DOMException("superseded", "AbortError"));
		},
		controller.signal,
	);
	assert.equal(state.signal, controller.signal);
	await assert.rejects(search, { name: "AbortError" });
	assert.deepEqual(seen, ["a"]);
});

test("events after the summary are ignored", async (t) => {
	reviewServe(t, [
		reviewBytes(
			[
				JSON.stringify({
					type: "done",
					files: 0,
					matches: 0,
					truncated: false,
				}),
				JSON.stringify({ type: "file", file: reviewFile("late") }),
				"",
			].join("\n"),
		),
	]);
	const seen: string[] = [];
	await streamWorkspaceSearch(reviewRequest, (file) => seen.push(file.path));
	assert.deepEqual(seen, []);
});
