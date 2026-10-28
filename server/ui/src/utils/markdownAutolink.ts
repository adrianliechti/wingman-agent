import type { MarkdownExtension } from "@tanstack/markdown";

// A URL in CommonMark <...> autolink form (which the parser leaves as text), or
// a bare http(s):// or www. URL. The bare www. form must not start mid-word
// ("runwww.example.com"). Only web schemes match, so hrefs need no sanitizing.
const URL_PATTERN =
	/<(https?:\/\/[^\s<>]+)>|(?<![\w.])(?:https?:\/\/|www\.)[^\s<>]+/gi;
// Sticky matching only inspects the current parser position, never a suffix scan.
const INLINE_URL_PATTERN = new RegExp(URL_PATTERN.source, "iy");

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

function matchUrl(match: RegExpExecArray): UrlMatch | undefined {
	const [whole, bracketed] = match;
	const url = bracketed ?? trimTrailing(whole);
	if (!HAS_HOST.test(url)) return undefined;
	const start = match.index + (bracketed ? 1 : 0);
	return {
		start,
		end: start + url.length,
		href: /^www\./i.test(url) ? `https://${url}` : url,
		bracketed: bracketed !== undefined,
	};
}

// Finds the URLs in a plain string. Shared by the markdown extension and the
// code renderer so both agree on URL boundaries.
export function findUrls(value: string): UrlMatch[] {
	if (!MAYBE_URL.test(value)) return [];

	const urls: UrlMatch[] = [];
	for (const match of value.matchAll(URL_PATTERN)) {
		const url = matchUrl(match);
		if (url) urls.push(url);
	}
	return urls;
}

// Parse URLs before emphasis can split their paths. The Markdown parser keeps
// code/escapes ahead of this hook and exposes link context to avoid nested links.
export function autolinkExtension(): MarkdownExtension {
	return {
		name: "autolink",
		inlineParser: {
			markers: "<hHwW",
			parse({ source, index, inLink }) {
				if (inLink) return undefined;
				INLINE_URL_PATTERN.lastIndex = index;
				const match = INLINE_URL_PATTERN.exec(source);
				const url = match && matchUrl(match);
				if (!url) return undefined;
				return {
					length: url.end + (url.bracketed ? 1 : 0) - index,
					node: {
						type: "link",
						href: url.href,
						children: [
							{ type: "text", value: source.slice(url.start, url.end) },
						],
					},
				};
			},
		},
	};
}
