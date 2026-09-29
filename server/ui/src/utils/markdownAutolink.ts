import type {
	InlineNode,
	MarkdownExtension,
	TextNode,
} from "@tanstack/markdown";

// A URL in CommonMark <...> autolink form (which the parser leaves as text), or
// a bare http(s):// or www. URL. The bare www. form must not start mid-word
// ("runwww.example.com"). Only web schemes match, so hrefs need no sanitizing.
const URL_PATTERN =
	/<(https?:\/\/[^\s<>]+)>|(?<![\w.])(?:https?:\/\/|www\.)[^\s<>]+/gi;

// Cheap pre-check so the common URL-free text node skips the regex scan.
const MAYBE_URL = /:\/\/|www\./i;

// Rejects matches that trimming reduced to a bare prefix like "www" or "https://".
const HAS_HOST = /^(?:https?:\/\/|www\.)[^/]/i;

// GFM trailing-punctuation rule: sentence punctuation and unbalanced closing
// parens at the end of a bare URL stay as prose.
const TRAILING_PUNCTUATION = "?!.,:;*_~'\"";

function trimTrailing(url: string): string {
	let unbalanced = url.split(")").length - url.split("(").length;
	let end = url.length;
	while (end > 0) {
		const char = url[end - 1];
		if (TRAILING_PUNCTUATION.includes(char)) {
			end--;
		} else if (char === ")" && unbalanced > 0) {
			end--;
			unbalanced--;
		} else {
			break;
		}
	}
	return url.slice(0, end);
}

export interface UrlMatch {
	// Range of the URL text itself, excluding any <...> wrapper.
	start: number;
	end: number;
	href: string;
	bracketed: boolean;
}

// Finds the URLs in a plain string. Shared by the markdown extension and the
// code renderer so both agree on URL boundaries.
export function findUrls(value: string): UrlMatch[] {
	if (!MAYBE_URL.test(value)) return [];

	const urls: UrlMatch[] = [];
	for (const match of value.matchAll(URL_PATTERN)) {
		const [whole, bracketed] = match;
		const url = bracketed ?? trimTrailing(whole);
		if (!HAS_HOST.test(url)) continue;

		const start = match.index + (bracketed ? 1 : 0);
		urls.push({
			start,
			end: start + url.length,
			href: /^www\./i.test(url) ? `https://${url}` : url,
			bracketed: bracketed !== undefined,
		});
	}
	return urls;
}

// Splits a text node into text and link nodes, dropping any <...> wrapper.
function linkifyText(node: TextNode): InlineNode | InlineNode[] {
	const { value } = node;
	const urls = findUrls(value);
	if (urls.length === 0) return node;

	const nodes: InlineNode[] = [];
	let cursor = 0;
	for (const { start, end, href, bracketed } of urls) {
		const from = bracketed ? start - 1 : start;
		if (from > cursor) {
			nodes.push({ type: "text", value: value.slice(cursor, from) });
		}
		nodes.push({
			type: "link",
			href,
			children: [{ type: "text", value: value.slice(start, end) }],
		});
		cursor = bracketed ? end + 1 : end;
	}
	if (cursor < value.length) {
		nodes.push({ type: "text", value: value.slice(cursor) });
	}
	return nodes;
}

// transformInline runs once per inline container, so nested children (strong,
// emphasis, ...) are walked here. Existing links are skipped to avoid nesting.
function transformNodes(nodes: InlineNode[]): InlineNode[] {
	return nodes.flatMap((node): InlineNode | InlineNode[] => {
		if (node.type === "text") return linkifyText(node);
		if ("children" in node && node.type !== "link") {
			return { ...node, children: transformNodes(node.children) };
		}
		return node;
	});
}

// Turns URLs in already-tokenized inline text into link nodes. Code spans and
// fences are separate node types by then, so URLs in code stay untouched.
// Known gap: emphasis is tokenized first, so a URL containing "*…*" or
// "/__x__" is split at the emphasis and only the part before it is linked.
export function autolinkExtension(): MarkdownExtension {
	return {
		name: "autolink",
		transformInline: transformNodes,
	};
}
