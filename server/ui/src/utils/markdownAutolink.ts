import type { InlineNode, MarkdownExtension } from "@tanstack/markdown";

// Matches a bare URL: an explicit http(s) scheme, or a "www." host that we
// promote to https. Kept intentionally narrow so the resulting href is always a
// safe web scheme and needs no extra sanitizing. The bare "www." form must sit
// at a word boundary (start, or after whitespace/punctuation) so it doesn't get
// picked out of the middle of a word like "runwww.example.com"; the "://" in the
// explicit scheme already prevents mid-word matches on its own.
const URL_PATTERN = /(?<![\w.])(https?:\/\/|www\.)[^\s<]+/gi;

// Trailing characters that are almost always sentence punctuation rather than
// part of the URL. Mirrors GFM's autolink trailing-punctuation rules.
const TRAILING_PUNCTUATION = new Set([
	"?",
	"!",
	".",
	",",
	":",
	";",
	"*",
	"_",
	"~",
	"'",
	'"',
]);

// Nodes that carry inline children we should recurse into. `link` is
// deliberately excluded so we never nest a link inside an existing one.
type InlineParent = Extract<InlineNode, { children: InlineNode[] }> & {
	type: "strong" | "emphasis" | "strike" | "inlineComponent";
};

function isInlineParent(node: InlineNode): node is InlineParent {
	return (
		node.type === "strong" ||
		node.type === "emphasis" ||
		node.type === "strike" ||
		node.type === "inlineComponent"
	);
}

// Peels trailing punctuation (and unbalanced closing parens) off a matched URL
// so text like "…/243 ( feat/x )" keeps the "(" as prose. Returns the URL
// without its trailing slice; that slice stays as surrounding text.
function trimTrailing(url: string): string {
	let end = url.length;
	while (end > 0) {
		const char = url[end - 1];
		if (TRAILING_PUNCTUATION.has(char)) {
			end--;
			continue;
		}
		if (char === ")") {
			const slice = url.slice(0, end);
			const opens = (slice.match(/\(/g) ?? []).length;
			const closes = (slice.match(/\)/g) ?? []).length;
			if (closes > opens) {
				end--;
				continue;
			}
		}
		break;
	}
	return url.slice(0, end);
}

// Splits a plain-text value into a mix of text and link nodes for any bare URLs
// it contains. Returns null when the text has no URL so callers can keep the
// original node reference.
function linkifyText(value: string): InlineNode[] | null {
	URL_PATTERN.lastIndex = 0;
	let match = URL_PATTERN.exec(value);
	if (!match) return null;

	const nodes: InlineNode[] = [];
	let cursor = 0;

	while (match) {
		const matchStart = match.index;
		const url = trimTrailing(match[0]);

		// trimTrailing can strip everything after the scheme (e.g. a lone "www."
		// followed only by punctuation); skip such empty matches.
		if (url.length > match[1].length) {
			if (matchStart > cursor) {
				nodes.push({ type: "text", value: value.slice(cursor, matchStart) });
			}
			const href = url.startsWith("www.") ? `https://${url}` : url;
			nodes.push({
				type: "link",
				href,
				children: [{ type: "text", value: url }],
			});
			cursor = matchStart + url.length;
			// Rescan from just after the URL so trailing punctuation is available
			// to the next match and stays as text.
			URL_PATTERN.lastIndex = cursor;
		}

		match = URL_PATTERN.exec(value);
	}

	if (nodes.length === 0) return null;
	if (cursor < value.length) {
		nodes.push({ type: "text", value: value.slice(cursor) });
	}
	return nodes;
}

function transformNodes(nodes: InlineNode[]): InlineNode[] {
	const result: InlineNode[] = [];
	let changed = false;

	for (const node of nodes) {
		if (node.type === "text") {
			const linked = linkifyText(node.value);
			if (linked) {
				result.push(...linked);
				changed = true;
			} else {
				result.push(node);
			}
			continue;
		}

		if (isInlineParent(node)) {
			const children = transformNodes(node.children);
			if (children === node.children) {
				result.push(node);
			} else {
				result.push({ ...node, children });
				changed = true;
			}
			continue;
		}

		result.push(node);
	}

	// Preserve the original reference when nothing changed so the renderer's
	// identity-based memoization keeps working.
	return changed ? result : nodes;
}

// A @tanstack/markdown extension that turns bare URLs in already-tokenized
// inline text into link nodes. Runs after code spans/fences are separated into
// their own node types, so URLs inside code are never linkified.
export function autolinkExtension(): MarkdownExtension {
	return {
		name: "autolink",
		transformInline(nodes) {
			return transformNodes(nodes);
		},
	};
}
