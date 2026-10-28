import assert from "node:assert/strict";
import test from "node:test";
import { parseInline, type InlineNode } from "@tanstack/markdown";
import { autolinkExtension, findUrls } from "../src/utils/markdownAutolink.ts";

const parse = (source: string) =>
	parseInline(source, { extensions: [autolinkExtension()] });

const text = (value: string): InlineNode => ({ type: "text", value });
const link = (url: string, href = url): InlineNode => ({
	type: "link",
	href,
	children: [text(url)],
});

const assertUnchanged = (source: string) =>
	assert.deepEqual(parse(source), [text(source)]);

test("linkifies a bare URL in the middle of text", () => {
	assert.deepEqual(parse("see https://example.com/foo for more"), [
		text("see "),
		link("https://example.com/foo"),
		text(" for more"),
	]);
});

test("keeps trailing punctuation and an opening paren as text", () => {
	assert.deepEqual(
		parse("party at https://example.com/wombat-rave/dance ( bring snacks )."),
		[
			text("party at "),
			link("https://example.com/wombat-rave/dance"),
			text(" ( bring snacks )."),
		],
	);
});

test("keeps balanced parens inside the URL path", () => {
	assert.deepEqual(parse("https://en.wikipedia.org/wiki/Go_(language)"), [
		link("https://en.wikipedia.org/wiki/Go_(language)"),
	]);
});

test("promotes a www. URL to an https href", () => {
	assert.deepEqual(parse("www.example.com/x"), [
		link("www.example.com/x", "https://www.example.com/x"),
	]);
});

test("promotes an uppercase WWW. URL to an https href", () => {
	assert.deepEqual(parse("WWW.Example.com"), [
		link("WWW.Example.com", "https://WWW.Example.com"),
	]);
});

test("does not linkify a bare www. glued to the middle of a word", () => {
	assertUnchanged("runwww.example.com is not a link");
});

test("linkifies a www. URL wrapped in parentheses", () => {
	assert.deepEqual(parse("(www.example.com/x)"), [
		text("("),
		link("www.example.com/x", "https://www.example.com/x"),
		text(")"),
	]);
});

test("unwraps a <url> autolink without leaking the brackets", () => {
	assert.deepEqual(parse("see <https://example.com> here"), [
		text("see "),
		link("https://example.com"),
		text(" here"),
	]);
});

test("ignores a bare scheme or www. with nothing after it", () => {
	assertUnchanged("try https:// or www.");
});

test("does not wrap a URL that is already inside a link", () => {
	assert.deepEqual(parse("[https://example.com](https://example.com)"), [
		link("https://example.com"),
	]);
});

test("linkifies URLs inside strong/emphasis children", () => {
	assert.deepEqual(parse("**go to https://example.com now**"), [
		{
			type: "strong",
			children: [text("go to "), link("https://example.com"), text(" now")],
		},
	]);
});

test("leaves text without a URL unchanged", () => {
	assertUnchanged("plain text, no links here");
});

test("findUrls reports the URL range without its <...> wrapper", () => {
	assert.deepEqual(findUrls("a <https://x.com> www.y.com."), [
		{ start: 3, end: 16, href: "https://x.com", bracketed: true },
		{ start: 18, end: 27, href: "https://www.y.com", bracketed: false },
	]);
});

test("parses complete URL paths before Markdown formatting changes them", () => {
	for (const url of [
		"https://example.com/__docs__/page",
		"https://example.com/*glob*/file",
		"https://example.com/a_b_c",
		"https://example.com/query?q=~value~&page=1",
	]) {
		assert.deepEqual(parse(url), [link(url)]);
	}
});

test("preserves code spans, explicit link labels and image alt text", () => {
	assert.deepEqual(parse("`https://example.com/__docs__`"), [
		{ type: "inlineCode", value: "https://example.com/__docs__" },
	]);
	assert.deepEqual(parse("[**https://example.com**](https://target.com)"), [
		{
			type: "link",
			href: "https://target.com",
			children: [{ type: "strong", children: [text("https://example.com")] }],
		},
	]);
	assert.deepEqual(parse("![https://example.com](https://image.com/a.png)"), [
		{
			type: "image",
			src: "https://image.com/a.png",
			alt: "https://example.com",
		},
	]);
});

test("ignores non-web schemes and preserves Unicode before URL offsets", () => {
	assertUnchanged("javascript:alert(1) data:text/html,test file:///tmp/a");
	assert.deepEqual(parse("🦀 https://example.com/__docs__/page"), [
		text("🦀 "),
		link("https://example.com/__docs__/page"),
	]);
});
