import assert from "node:assert/strict";
import test from "node:test";
import type { InlineNode } from "@tanstack/markdown";
import { autolinkExtension, findUrls } from "../src/utils/markdownAutolink.ts";

const transform = (nodes: InlineNode[]) =>
	autolinkExtension().transformInline!(nodes, {
		options: {},
	});

const text = (value: string): InlineNode => ({ type: "text", value });
const link = (url: string, href = url): InlineNode => ({
	type: "link",
	href,
	children: [text(url)],
});

const assertUnchanged = (...nodes: InlineNode[]) =>
	assert.deepEqual(transform(nodes), nodes);

test("linkifies a bare URL in the middle of text", () => {
	assert.deepEqual(transform([text("see https://example.com/foo for more")]), [
		text("see "),
		link("https://example.com/foo"),
		text(" for more"),
	]);
});

test("keeps trailing punctuation and an opening paren as text", () => {
	assert.deepEqual(
		transform([
			text("party at https://example.com/wombat-rave/dance ( bring snacks )."),
		]),
		[
			text("party at "),
			link("https://example.com/wombat-rave/dance"),
			text(" ( bring snacks )."),
		],
	);
});

test("keeps balanced parens inside the URL path", () => {
	assert.deepEqual(
		transform([text("https://en.wikipedia.org/wiki/Go_(language)")]),
		[link("https://en.wikipedia.org/wiki/Go_(language)")],
	);
});

test("promotes a www. URL to an https href", () => {
	assert.deepEqual(transform([text("www.example.com/x")]), [
		link("www.example.com/x", "https://www.example.com/x"),
	]);
});

test("promotes an uppercase WWW. URL to an https href", () => {
	assert.deepEqual(transform([text("WWW.Example.com")]), [
		link("WWW.Example.com", "https://WWW.Example.com"),
	]);
});

test("does not linkify a bare www. glued to the middle of a word", () => {
	assertUnchanged(text("runwww.example.com is not a link"));
});

test("linkifies a www. URL wrapped in parentheses", () => {
	assert.deepEqual(transform([text("(www.example.com/x)")]), [
		text("("),
		link("www.example.com/x", "https://www.example.com/x"),
		text(")"),
	]);
});

test("unwraps a <url> autolink without leaking the brackets", () => {
	assert.deepEqual(transform([text("see <https://example.com> here")]), [
		text("see "),
		link("https://example.com"),
		text(" here"),
	]);
});

test("ignores a bare scheme or www. with nothing after it", () => {
	assertUnchanged(text("try https:// or www."));
});

test("does not wrap a URL that is already inside a link", () => {
	assertUnchanged(link("https://example.com"));
});

test("linkifies URLs inside strong/emphasis children", () => {
	assert.deepEqual(
		transform([
			{ type: "strong", children: [text("go to https://example.com now")] },
		]),
		[
			{
				type: "strong",
				children: [text("go to "), link("https://example.com"), text(" now")],
			},
		],
	);
});

test("leaves text without a URL unchanged", () => {
	assertUnchanged(text("plain text, no links here"));
});

test("findUrls reports the URL range without its <...> wrapper", () => {
	assert.deepEqual(findUrls("a <https://x.com> www.y.com."), [
		{ start: 3, end: 16, href: "https://x.com", bracketed: true },
		{ start: 18, end: 27, href: "https://www.y.com", bracketed: false },
	]);
});
