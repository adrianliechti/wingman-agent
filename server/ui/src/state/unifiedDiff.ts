type Op = { kind: " " | "-" | "+"; line: string };

// Beyond this many edits the middle of a file is shown as replaced; the
// Myers trace grows quadratically with the edit count.
const MAX_EDITS = 2000;

/** Lines keep their terminator so a missing final newline is a change. */
function lines(text: string): string[] {
	return text ? text.split(/(?<=\n)/) : [];
}

function myers(a: string[], b: string[]): Op[] {
	const n = a.length,
		m = b.length,
		max = n + m;
	const replaced = (): Op[] => [
		...a.map((line) => ({ kind: "-" as const, line })),
		...b.map((line) => ({ kind: "+" as const, line })),
	];
	if (!n || !m) return replaced();
	const v = new Int32Array(2 * max + 2);
	// Step d only reads the previous step's diagonals, k in [1-d, d-1].
	const trace: Int32Array[] = [];
	for (let d = 0; d <= max; d++) {
		if (d > MAX_EDITS) return replaced();
		trace.push(v.slice(max - d + 1, max + d));
		for (let k = -d; k <= d; k += 2) {
			let x =
				k === -d || (k !== d && v[max + k - 1] < v[max + k + 1])
					? v[max + k + 1]
					: v[max + k - 1] + 1;
			let y = x - k;
			while (x < n && y < m && a[x] === b[y]) {
				x++;
				y++;
			}
			v[max + k] = x;
			if (x >= n && y >= m) return backtrack(trace, a, b);
		}
	}
	return replaced();
}

function backtrack(trace: Int32Array[], a: string[], b: string[]): Op[] {
	const ops: Op[] = [];
	let x = a.length,
		y = b.length;
	const diagonal = (px: number, py: number) => {
		while (x > px && y > py) {
			ops.push({ kind: " ", line: a[x - 1] });
			x--;
			y--;
		}
	};
	for (let d = trace.length - 1; d > 0; d--) {
		const v = trace[d];
		const at = (k: number) => v[k + d - 1];
		const k = x - y;
		const previous =
			k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
		const px = at(previous);
		diagonal(px, px - previous);
		if (x === px) ops.push({ kind: "+", line: b[--y] });
		else ops.push({ kind: "-", line: a[--x] });
	}
	diagonal(0, 0);
	return ops.reverse();
}

function diffLines(a: string[], b: string[]): Op[] {
	let start = 0;
	while (start < a.length && start < b.length && a[start] === b[start]) start++;
	let endA = a.length,
		endB = b.length;
	while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
		endA--;
		endB--;
	}
	const context = (line: string): Op => ({ kind: " ", line });
	return [
		...a.slice(0, start).map(context),
		...myers(a.slice(start, endA), b.slice(start, endB)),
		...a.slice(endA).map(context),
	];
}

function range(start: number, count: number): string {
	return count === 1 ? `${start}` : `${start},${count}`;
}

function hunks(ops: Op[], context: number): string[] {
	const out: string[] = [];
	const oldAt: number[] = [],
		newAt: number[] = [];
	let o = 0,
		n = 0;
	for (const op of ops) {
		oldAt.push(o);
		newAt.push(n);
		if (op.kind !== "+") o++;
		if (op.kind !== "-") n++;
	}
	let i = 0;
	while (i < ops.length) {
		while (i < ops.length && ops[i].kind === " ") i++;
		if (i >= ops.length) break;
		const start = Math.max(0, i - context);
		let end = i,
			j = i;
		while (j < ops.length) {
			if (ops[j].kind !== " ") {
				end = ++j;
				continue;
			}
			let k = j;
			while (k < ops.length && ops[k].kind === " ") k++;
			if (k < ops.length && k - j <= 2 * context) j = k;
			else break;
		}
		const stop = Math.min(ops.length, end + context);
		const slice = ops.slice(start, stop);
		const oldCount = slice.filter((op) => op.kind !== "+").length,
			newCount = slice.filter((op) => op.kind !== "-").length;
		out.push(
			`@@ -${range(oldCount ? oldAt[start] + 1 : oldAt[start], oldCount)} +${range(newCount ? newAt[start] + 1 : newAt[start], newCount)} @@`,
		);
		for (const op of slice) {
			const text = op.line.endsWith("\n") ? op.line.slice(0, -1) : op.line;
			out.push(op.kind + text);
			if (!op.line.endsWith("\n")) out.push("\\ No newline at end of file");
		}
		i = stop;
	}
	return out;
}

export type DiffSide = { path: string; content?: string };

/** A Git-style unified diff between two file versions. An absent side is an
 * added or deleted file; `binary` replaces the hunks with Git's summary line. */
export function unifiedDiff(
	before: DiffSide | undefined,
	after: DiffSide | undefined,
	{ binary = false, context = 3 }: { binary?: boolean; context?: number } = {},
): string {
	const oldPath = before?.path ?? after!.path,
		newPath = after?.path ?? before!.path;
	const header = [`diff --git a/${oldPath} b/${newPath}`];
	if (before && after && oldPath !== newPath)
		header.push(`rename from ${oldPath}`, `rename to ${newPath}`);
	const oldName = before ? `a/${oldPath}` : "/dev/null",
		newName = after ? `b/${newPath}` : "/dev/null";
	if (binary) {
		header.push(`Binary files ${oldName} and ${newName} differ`);
		return header.join("\n") + "\n";
	}
	const body = hunks(
		diffLines(lines(before?.content ?? ""), lines(after?.content ?? "")),
		context,
	);
	if (!body.length) return header.join("\n") + "\n";
	return (
		[...header, `--- ${oldName}`, `+++ ${newName}`, ...body].join("\n") + "\n"
	);
}
