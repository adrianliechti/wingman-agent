import assert from "node:assert/strict";
import test from "node:test";
import { unifiedDiff } from "../src/state/unifiedDiff.ts";

function reviewLines(text: string) {
	return text ? text.split(/(?<=\n)/) : [];
}
/** Applies a unified diff the way `git apply` checks it: exact hunk ranges,
 * matching context and end-of-file markers only after a file's last line. */
function reviewApply(before: string, patch: string): string {
	const old = reviewLines(before);
	const rows = patch.split("\n");
	assert.equal(rows.pop(), "", "a patch ends with a newline");
	let i = rows.findIndex((row) => row.startsWith("@@"));
	if (i < 0) return before;
	const out: string[] = [];
	let cursor = 0;
	while (i < rows.length) {
		const m = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@$/.exec(rows[i]);
		assert.ok(m, `hunk header ${rows[i]}`);
		const oldCount = m[2] === undefined ? 1 : +m[2],
			newCount = m[4] === undefined ? 1 : +m[4];
		const from = oldCount ? +m[1] - 1 : +m[1];
		assert.ok(from >= cursor, "hunks are ordered and disjoint");
		out.push(...old.slice(cursor, from));
		assert.equal(
			out.length,
			newCount ? +m[3] - 1 : +m[3],
			`new start of ${rows[i]}`,
		);
		cursor = from;
		let o = 0,
			n = 0,
			side = "";
		for (i++; i < rows.length && !rows[i].startsWith("@@"); i++) {
			const row = rows[i];
			if (row.startsWith("\\")) {
				assert.equal(row, "\\ No newline at end of file");
				if (side !== "-") {
					assert.ok(out.at(-1)!.endsWith("\n"));
					out.push(out.pop()!.slice(0, -1));
				}
				if (side !== "+")
					assert.equal(cursor, old.length, "old marker after its last line");
				continue;
			}
			side = row[0];
			const line = row.slice(1);
			if (side !== "+") {
				assert.ok(
					old[cursor] === `${line}\n` || old[cursor] === line,
					`context ${JSON.stringify(old[cursor])} vs ${JSON.stringify(line)}`,
				);
				cursor++;
				o++;
			}
			if (side !== "-") {
				out.push(`${line}\n`);
				n++;
			}
			assert.ok(" -+".includes(side), `row ${JSON.stringify(row)}`);
		}
		assert.deepEqual([o, n], [oldCount, newCount], "hunk counts");
	}
	return [...out, ...old.slice(cursor)].join("");
}
function reviewBody(patch: string) {
	return patch
		.split("\n")
		.filter((row) => /^[-+ @\\]/.test(row) && !/^(---|\+\+\+) /.test(row));
}

test("random edits produce git-applicable hunks for every context size and ending", () => {
	let seed = 20261031;
	const random = () => (seed = (seed * 48271) % 2147483647) / 2147483647;
	const alphabet = ["a", "b", "c", "", "a\r", " a", "\\ x", "--- y", "+++ z"];
	const pick = () => alphabet[Math.floor(random() * alphabet.length)];
	for (let round = 0; round < 4000; round++) {
		const before = Array.from({ length: Math.floor(random() * 16) }, pick);
		const after =
			random() < 0.2
				? Array.from({ length: Math.floor(random() * 16) }, pick)
				: before
						.filter(() => random() > 0.3)
						.flatMap((line) => (random() > 0.7 ? [line, pick()] : [line]));
		const a = before.join("\n") + (before.length && random() < 0.5 ? "\n" : "");
		const b = after.join("\n") + (after.length && random() < 0.5 ? "\n" : "");
		const context = [0, 1, 3, 1e9][round % 4];
		const patch = unifiedDiff(
			{ path: "f", content: a },
			{ path: "f", content: b },
			{ context },
		);
		assert.equal(
			reviewApply(a, patch),
			b,
			`${JSON.stringify(a)} -> ${JSON.stringify(b)}\n${patch}`,
		);
		if (a === b) assert.equal(patch, "diff --git a/f b/f\n");
	}
});

test("hunks keep at most the requested context around each change", () => {
	const lines = Array.from({ length: 40 }, (_, i) => `l${i}\n`);
	const changed = lines.map((line, i) =>
		i === 10 || i === 17 || i === 25 ? `x${i}\n` : line,
	);
	const patch = unifiedDiff(
		{ path: "f", content: lines.join("") },
		{ path: "f", content: changed.join("") },
	);
	assert.deepEqual(
		patch.split("\n").filter((row) => row.startsWith("@@")),
		// Six unchanged lines between changes join a hunk; seven split it.
		["@@ -8,14 +8,14 @@", "@@ -23,7 +23,7 @@"],
	);
	const none = unifiedDiff(
		{ path: "f", content: lines.join("") },
		{ path: "f", content: changed.join("") },
		{ context: 0 },
	);
	assert.deepEqual(
		none.split("\n").filter((row) => row.startsWith("@@")),
		["@@ -11 +11 @@", "@@ -18 +18 @@", "@@ -26 +26 @@"],
	);
});

test("line ending changes are content changes and keep their carriage returns", () => {
	const patch = unifiedDiff(
		{ path: "w", content: "a\r\nb\r\n" },
		{ path: "w", content: "a\nb\r\n" },
	);
	assert.deepEqual(reviewBody(patch), [
		"@@ -1,2 +1,2 @@",
		"-a\r",
		"+a",
		" b\r",
	]);
	const lone = unifiedDiff(
		{ path: "w", content: "a\rb" },
		{ path: "w", content: "a\rc" },
	);
	assert.deepEqual(reviewBody(lone), [
		"@@ -1 +1 @@",
		"-a\rb",
		"\\ No newline at end of file",
		"+a\rc",
		"\\ No newline at end of file",
	]);
});

test("empty files, an added final newline and a lone newline", () => {
	assert.equal(
		unifiedDiff({ path: "e", content: "" }, { path: "e", content: "" }),
		"diff --git a/e b/e\n",
	);
	assert.deepEqual(
		reviewBody(
			unifiedDiff({ path: "e", content: "" }, { path: "e", content: "\n" }),
		),
		["@@ -0,0 +1 @@", "+"],
	);
	assert.deepEqual(
		reviewBody(unifiedDiff(undefined, { path: "e", content: "" })),
		[],
	);
	assert.equal(
		unifiedDiff({ path: "e", content: "x" }, undefined),
		"diff --git a/e b/e\n--- a/e\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n\\ No newline at end of file\n",
	);
	const appended = unifiedDiff(
		{ path: "e", content: "a\nb" },
		{ path: "e", content: "a\nb\nc\n" },
	);
	assert.equal(reviewApply("a\nb", appended), "a\nb\nc\n");
});

test("a renamed and edited file keeps both names; binary changes only summarize", () => {
	const patch = unifiedDiff(
		{ path: "dir/old name.ts", content: "one\ntwo\n" },
		{ path: "dir/new name.ts", content: "one\n2\n" },
	);
	assert.deepEqual(patch.split("\n").slice(0, 5), [
		"diff --git a/dir/old name.ts b/dir/new name.ts",
		"rename from dir/old name.ts",
		"rename to dir/new name.ts",
		"--- a/dir/old name.ts",
		"+++ b/dir/new name.ts",
	]);
	assert.equal(reviewApply("one\ntwo\n", patch), "one\n2\n");
	assert.equal(
		unifiedDiff({ path: "a.bin" }, { path: "b.bin" }, { binary: true }),
		"diff --git a/a.bin b/b.bin\nrename from a.bin\nrename to b.bin\nBinary files a/a.bin and b/b.bin differ\n",
	);
});

test("large files stay fast and correct, beyond the edit budget too", () => {
	const lines = Array.from({ length: 60_000 }, (_, i) => `line ${i}\n`);
	const before = lines.join("");
	const cases = [
		lines.map((line, i) => (i === 30_000 ? "changed\n" : line)),
		lines.map((line, i) => (i % 40 === 0 ? `changed ${i}\n` : line)),
		lines.map((_, i) => `other ${i}\n`),
	];
	for (const changed of cases) {
		const after = changed.join("");
		const started = performance.now();
		const patch = unifiedDiff(
			{ path: "big", content: before },
			{ path: "big", content: after },
		);
		assert.ok(
			performance.now() - started < 5000,
			"diffing a large file is bounded",
		);
		assert.equal(reviewApply(before, patch), after);
	}
});
