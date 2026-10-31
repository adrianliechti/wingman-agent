import assert from "node:assert/strict";
import test from "node:test";
import { unifiedDiff } from "../src/state/unifiedDiff.ts";

const numbered = (count: number, change?: Record<number, string>) =>
	Array.from({ length: count }, (_, i) => change?.[i + 1] ?? `line ${i + 1}`)
		.map((line) => `${line}\n`)
		.join("");

test("modified files get Git hunks with three lines of context", () => {
	const patch = unifiedDiff(
		{ path: "a.txt", content: numbered(20) },
		{ path: "a.txt", content: numbered(20, { 5: "five", 16: "sixteen" }) },
	);
	assert.equal(
		patch,
		[
			"diff --git a/a.txt b/a.txt",
			"--- a/a.txt",
			"+++ b/a.txt",
			"@@ -2,7 +2,7 @@",
			" line 2",
			" line 3",
			" line 4",
			"-line 5",
			"+five",
			" line 6",
			" line 7",
			" line 8",
			"@@ -13,7 +13,7 @@",
			" line 13",
			" line 14",
			" line 15",
			"-line 16",
			"+sixteen",
			" line 17",
			" line 18",
			" line 19",
			"",
		].join("\n"),
	);
});

test("nearby changes share a hunk and insertions keep their position", () => {
	const patch = unifiedDiff(
		{ path: "a.txt", content: "a\nb\nc\nd\n" },
		{ path: "a.txt", content: "a\nx\nb\nc\nd\ny\n" },
	);
	assert.equal(
		patch.split("\n").slice(3).join("\n"),
		["@@ -1,4 +1,6 @@", " a", "+x", " b", " c", " d", "+y", ""].join("\n"),
	);
});

test("added, deleted and renamed files use Git headers", () => {
	assert.equal(
		unifiedDiff(undefined, { path: "new.txt", content: "one\ntwo" }),
		[
			"diff --git a/new.txt b/new.txt",
			"--- /dev/null",
			"+++ b/new.txt",
			"@@ -0,0 +1,2 @@",
			"+one",
			"+two",
			"\\ No newline at end of file",
			"",
		].join("\n"),
	);
	assert.equal(
		unifiedDiff({ path: "old.txt", content: "gone\n" }, undefined),
		[
			"diff --git a/old.txt b/old.txt",
			"--- a/old.txt",
			"+++ /dev/null",
			"@@ -1 +0,0 @@",
			"-gone",
			"",
		].join("\n"),
	);
	assert.equal(
		unifiedDiff(
			{ path: "from.txt", content: "same\n" },
			{ path: "to.txt", content: "same\n" },
		),
		"diff --git a/from.txt b/to.txt\nrename from from.txt\nrename to to.txt\n",
	);
});

test("binary files and a changed final newline", () => {
	assert.equal(
		unifiedDiff({ path: "image.png" }, { path: "image.png" }, { binary: true }),
		"diff --git a/image.png b/image.png\nBinary files a/image.png and b/image.png differ\n",
	);
	assert.equal(
		unifiedDiff(undefined, { path: "x.bin" }, { binary: true }).split("\n")[1],
		"Binary files /dev/null and b/x.bin differ",
	);
	assert.equal(
		unifiedDiff(
			{ path: "a", content: "x\ny" },
			{ path: "a", content: "x\ny\n" },
		)
			.split("\n")
			.slice(3)
			.join("\n"),
		[
			"@@ -1,2 +1,2 @@",
			" x",
			"-y",
			"\\ No newline at end of file",
			"+y",
			"",
		].join("\n"),
	);
});

test("the edit script reproduces the new file for arbitrary edits", () => {
	let seed = 7;
	const random = () => (seed = (seed * 48271) % 2147483647) / 2147483647;
	for (let round = 0; round < 50; round++) {
		const before = Array.from({ length: 40 }, () =>
			String.fromCharCode(97 + Math.floor(random() * 4)),
		);
		const after = before
			.filter(() => random() > 0.2)
			.flatMap((line) => (random() > 0.8 ? [line, "z"] : [line]));
		const patch = unifiedDiff(
			{ path: "f", content: before.join("\n") + "\n" },
			{ path: "f", content: after.join("\n") + "\n" },
			{ context: 1e9 },
		);
		const body = patch.split("\n").slice(4, -1);
		assert.deepEqual(
			body.filter((line) => !line.startsWith("+")).map((line) => line.slice(1)),
			before,
		);
		assert.deepEqual(
			body.filter((line) => !line.startsWith("-")).map((line) => line.slice(1)),
			after,
		);
	}
});
