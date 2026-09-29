import assert from "node:assert/strict";
import test from "node:test";
import type { InlineNode } from "@tanstack/markdown";
import { autolinkExtension } from "../src/utils/markdownAutolink.ts";

const transform = (nodes: InlineNode[]) =>
	autolinkExtension().transformInline!(nodes, {
		options: {},
	});

test("linkifies a bare URL in the middle of text", () => {
	const result = transform([
		{ type: "text", value: "see https://example.com/foo for more" },
	]);
	assert.deepEqual(result, [
		{ type: "text", value: "see " },
		{
			type: "link",
			href: "https://example.com/foo",
			children: [{ type: "text", value: "https://example.com/foo" }],
		},
		{ type: "text", value: " for more" },
	]);
});

test("keeps trailing punctuation and an opening paren as text", () => {
	const result = transform([
		{
			type: "text",
			value: "party at https://example.com/wombat-rave/dance ( bring snacks ).",
		},
	]);
	assert.deepEqual(result, [
		{ type: "text", value: "party at " },
		{
			type: "link",
			href: "https://example.com/wombat-rave/dance",
			children: [
				{ type: "text", value: "https://example.com/wombat-rave/dance" },
			],
		},
		{ type: "text", value: " ( bring snacks )." },
	]);
});

test("keeps balanced parens inside the URL path", () => {
	const result = transform([
		{ type: "text", value: "https://en.wikipedia.org/wiki/Go_(language)" },
	]);
	assert.deepEqual(result, [
		{
			type: "link",
			href: "https://en.wikipedia.org/wiki/Go_(language)",
			children: [
				{ type: "text", value: "https://en.wikipedia.org/wiki/Go_(language)" },
			],
		},
	]);
});

test("promotes a www. URL to an https href", () => {
	const result = transform([{ type: "text", value: "www.example.com/x" }]);
	assert.deepEqual(result, [
		{
			type: "link",
			href: "https://www.example.com/x",
			children: [{ type: "text", value: "www.example.com/x" }],
		},
	]);
});

test("does not linkify a bare www. glued to the middle of a word", () => {
	const nodes: InlineNode[] = [
		{ type: "text", value: "runwww.example.com is not a link" },
	];
	const result = transform(nodes);
	assert.equal(result, nodes);
});

test("linkifies a www. URL wrapped in parentheses", () => {
	const result = transform([{ type: "text", value: "(www.example.com/x)" }]);
	assert.deepEqual(result, [
		{ type: "text", value: "(" },
		{
			type: "link",
			href: "https://www.example.com/x",
			children: [{ type: "text", value: "www.example.com/x" }],
		},
		{ type: "text", value: ")" },
	]);
});

test("does not wrap a URL that is already inside a link", () => {
	const nodes: InlineNode[] = [
		{
			type: "link",
			href: "https://example.com",
			children: [{ type: "text", value: "https://example.com" }],
		},
	];
	const result = transform(nodes);
	assert.equal(result, nodes);
});

test("linkifies URLs inside strong/emphasis children", () => {
	const result = transform([
		{
			type: "strong",
			children: [{ type: "text", value: "go to https://example.com now" }],
		},
	]);
	assert.deepEqual(result, [
		{
			type: "strong",
			children: [
				{ type: "text", value: "go to " },
				{
					type: "link",
					href: "https://example.com",
					children: [{ type: "text", value: "https://example.com" }],
				},
				{ type: "text", value: " now" },
			],
		},
	]);
});

test("returns the same array reference when there is no URL", () => {
	const nodes: InlineNode[] = [
		{ type: "text", value: "plain text, no links here" },
	];
	const result = transform(nodes);
	assert.equal(result, nodes);
});
