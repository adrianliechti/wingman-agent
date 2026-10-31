import assert from "node:assert/strict";
import test from "node:test";
import { chatReducer } from "@microsoft/agent-host-protocol";
import { AHPSessionProjection } from "../src/state/ahpSession.ts";

const reviewChatURI = "ahp-chat:/review";
const reviewRoot: any = {
	agents: [
		{
			provider: "one",
			displayName: "One",
			description: "",
			models: [
				{ id: "small", provider: "one", name: "Small", maxContextWindow: 1000 },
				{ id: "large", provider: "one", name: "Large", maxContextWindow: 8000 },
			],
		},
	],
};
const reviewSession: any = {
	provider: "one",
	title: "Review",
	status: 1,
	lifecycle: "ready",
	activeClients: [],
	chats: [
		{ resource: reviewChatURI, title: "Review", status: 1, modifiedAt: "" },
	],
	defaultChat: reviewChatURI,
	config: {
		schema: {
			type: "object",
			properties: {
				model: { type: "string", title: "Model", enum: ["small", "large"] },
			},
		},
		values: { model: "small" },
	},
};
const reviewUser = (text: string) => ({ text, origin: { kind: "user" } });

/** Applies actions through the official reducer, like AHPConnection does. */
function reviewRun(actions: any[], initial?: any) {
	let state: any = initial ?? {
		resource: reviewChatURI,
		title: "Review",
		status: 1,
		modifiedAt: "",
		turns: [],
	};
	for (const action of actions) state = chatReducer(state, action);
	return state;
}

test("a tool call keeps one entry from streaming through approval to its result", () => {
	const projection = new AHPSessionProjection("file:///work");
	const view = (chat: any, revision = 1) =>
		projection.project(reviewChatURI, reviewSession, chat, revision, {
			root: reviewRoot,
		});
	let chat = reviewRun([
		{
			type: "chat/turnStarted",
			turnId: "t1",
			startedAt: "2026-01-01T00:00:00Z",
			message: reviewUser("go"),
		},
		{
			type: "chat/responsePart",
			turnId: "t1",
			part: { kind: "markdown", id: "m1", content: "Look" },
		},
		{
			type: "chat/toolCallStart",
			turnId: "t1",
			toolCallId: "c1",
			toolName: "bash",
			displayName: "Bash",
			intention: "List files",
			_meta: { toolKind: "terminal" },
		},
		{
			type: "chat/toolCallDelta",
			turnId: "t1",
			toolCallId: "c1",
			content: '{"cmd":',
		},
	]);
	let current = view(chat);
	const markdown = current.entries[1];
	assert.equal(current.phase, "tool_running");
	assert.deepEqual(
		{ ...current.entries[2], id: undefined },
		{
			id: undefined,
			toolId: "c1",
			type: "tool",
			content: "",
			toolName: "bash",
			toolKind: "execute",
			toolHint: "List files",
			toolArgs: '{"cmd":',
			toolPartial: true,
			toolResult: undefined,
		},
	);
	chat = reviewRun(
		[
			{
				type: "chat/toolCallReady",
				turnId: "t1",
				toolCallId: "c1",
				invocationMessage: "Run ls",
				toolInput: '{"cmd":"ls"}',
				options: [
					{ id: "once", label: "Allow", kind: "approve" },
					{ id: "deny", label: "Deny", kind: "deny" },
				],
			},
		],
		chat,
	);
	current = view(chat, 2);
	assert.equal(
		current.entries[1],
		markdown,
		"unchanged parts keep their identity",
	);
	assert.equal(current.entries[2].toolArgs, '{"cmd":"ls"}');
	assert.equal(current.entries[2].toolPartial, false);
	assert.deepEqual(current.prompts, [
		{
			id: "c1",
			kind: "confirm",
			message: "Run ls",
			options: [
				{ id: "once", label: "Allow", kind: "approve" },
				{ id: "deny", label: "Deny", kind: "deny" },
			],
		},
	]);
	chat = reviewRun(
		[
			{
				type: "chat/toolCallConfirmed",
				turnId: "t1",
				toolCallId: "c1",
				approved: true,
				confirmed: "user-action",
				selectedOptionId: "once",
			},
			{
				type: "chat/toolCallContentChanged",
				turnId: "t1",
				toolCallId: "c1",
				content: [{ type: "text", text: "partial" }],
			},
		],
		chat,
	);
	current = view(chat, 3);
	assert.deepEqual(current.prompts, []);
	assert.equal(
		current.entries[2].toolResult,
		undefined,
		"a running tool has no result yet",
	);
	chat = reviewRun(
		[
			{
				type: "chat/toolCallComplete",
				turnId: "t1",
				toolCallId: "c1",
				result: {
					success: true,
					pastTenseMessage: "Ran ls",
					content: [
						{ type: "text", text: "a.txt" },
						{
							type: "resource",
							uri: "file:///work/src/a%20b.ts#L12",
							label: "a b.ts",
						},
						{ type: "resource", uri: "file:///elsewhere/x.ts", label: "x.ts" },
						{
							type: "embeddedResource",
							label: "png",
							contentType: "image/png",
							data: "AAAA",
						},
					],
				},
			},
			{
				type: "chat/usage",
				turnId: "t1",
				usage: {
					inputTokens: 600,
					outputTokens: 20,
					cacheReadTokens: 5,
					model: "large",
				},
			},
			{ type: "chat/turnComplete", turnId: "t1", duration: 10 },
		],
		chat,
	);
	current = view(chat, 4);
	assert.equal(current.phase, "idle");
	assert.equal(current.error, null);
	const tool = current.entries[2];
	assert.equal(tool.toolResult, "a.txt");
	assert.deepEqual(tool.toolLocations, [
		{ path: "src/a b.ts", line: 12 },
		{ path: "/elsewhere/x.ts" },
	]);
	assert.deepEqual(tool.images, ["data:image/png;base64,AAAA"]);
	assert.equal(
		current.entries[1],
		markdown,
		"a completed turn reuses its parts' entries",
	);
	assert.equal(current.usage.lastInputTokens, 600);
	assert.equal(
		current.usage.contextWindow,
		8000,
		"the turn's model sizes the meter",
	);
});

test("ending a turn mid-call settles every tool and reports the outcome", () => {
	const projection = new AHPSessionProjection();
	const started = reviewRun([
		{
			type: "chat/turnStarted",
			turnId: "t1",
			startedAt: "2026-01-01T00:00:00Z",
			message: reviewUser("go"),
		},
		{
			type: "chat/toolCallStart",
			turnId: "t1",
			toolCallId: "streaming",
			toolName: "write",
			displayName: "Write",
		},
		{
			type: "chat/toolCallDelta",
			turnId: "t1",
			toolCallId: "streaming",
			content: "{",
		},
		{
			type: "chat/toolCallStart",
			turnId: "t1",
			toolCallId: "approval:7",
			toolName: "bash",
			displayName: "Bash",
		},
		{
			type: "chat/toolCallReady",
			turnId: "t1",
			toolCallId: "approval:7",
			invocationMessage: "Run",
			toolInput: "x",
		},
	]);
	const before = projection.project(reviewChatURI, reviewSession, started, 1);
	assert.deepEqual(
		before.entries.map((entry) => entry.id),
		["user:t1", "streaming"],
		"approval tool calls are prompts, not transcript entries",
	);
	assert.deepEqual(
		before.prompts.map((prompt) => prompt.id),
		["approval:7"],
	);
	const cancelled = projection.project(
		reviewChatURI,
		reviewSession,
		reviewRun(
			[{ type: "chat/turnCancelled", turnId: "t1", duration: 1 }],
			started,
		),
		2,
	);
	assert.equal(cancelled.phase, "idle");
	assert.equal(cancelled.error, "Cancelled");
	assert.deepEqual(cancelled.prompts, []);
	assert.equal(
		cancelled.entries[1].toolResult,
		"",
		"a skipped call is no longer pending",
	);
	assert.equal(cancelled.entries[1].toolPartial, false);
	const failed = projection.project(
		reviewChatURI,
		reviewSession,
		reviewRun(
			[
				{
					type: "chat/error",
					turnId: "t1",
					duration: 1,
					part: {
						kind: "error",
						error: { errorType: "agent", message: "rate limited" },
					},
				},
			],
			started,
		),
		3,
	);
	assert.equal(failed.error, "rate limited");
	const recovered = projection.project(
		reviewChatURI,
		reviewSession,
		reviewRun(
			[
				{
					type: "chat/error",
					turnId: "t1",
					duration: 1,
					part: { kind: "error", error: { errorType: "agent", message: "x" } },
				},
				{
					type: "chat/turnStarted",
					turnId: "t2",
					startedAt: "2026-01-01T00:00:01Z",
					message: reviewUser("again"),
				},
			],
			started,
		),
		4,
	);
	assert.equal(
		recovered.error,
		null,
		"a running turn clears the last turn's error",
	);
	assert.equal(recovered.phase, "thinking");
});

test("steering, queued and compaction turns project into the queue and notices", () => {
	const projection = new AHPSessionProjection();
	let chat = reviewRun([
		{
			type: "chat/turnStarted",
			turnId: "t1",
			startedAt: "2026-01-01T00:00:00Z",
			message: reviewUser("work"),
		},
		{
			type: "chat/pendingMessageSet",
			kind: "queued",
			id: "q1",
			message: reviewUser("next"),
		},
		{
			type: "chat/pendingMessageSet",
			kind: "steering",
			id: "s1",
			message: reviewUser("hurry"),
		},
	]);
	assert.equal(chat.steeringMessage.id, "s1");
	assert.equal(
		chat.queuedMessages.length,
		2,
		"1.1 keeps steering in the ordered queue too",
	);
	let view = projection.project(reviewChatURI, reviewSession, chat, 1);
	assert.deepEqual(
		view.pendingInputs.map((input) => [input.id, input.intent]),
		[
			["s1", "steer"],
			["q1", "follow_up"],
		],
	);
	chat = reviewRun(
		[
			{ type: "chat/pendingMessageRemoved", kind: "steering", id: "s1" },
			{
				type: "chat/responsePart",
				turnId: "t1",
				part: { kind: "systemNotification", content: "hurry" },
			},
			{ type: "chat/turnComplete", turnId: "t1", duration: 1 },
			{
				type: "chat/turnStarted",
				turnId: "t2",
				startedAt: "2026-01-01T00:00:02Z",
				message: reviewUser("/compact"),
				queuedMessageId: "q1",
			},
			{ type: "chat/activityChanged", activity: "Compacting context…" },
		],
		chat,
	);
	view = projection.project(reviewChatURI, reviewSession, chat, 2);
	assert.deepEqual(view.pendingInputs, []);
	assert.equal(view.activity, "Compacting context…");
	assert.deepEqual(
		view.entries.map((entry) => [entry.type, entry.content]),
		[
			["user", "work"],
			["notice", "hurry"],
			["user", "/compact"],
		],
	);
	chat = reviewRun(
		[
			{ type: "chat/activityChanged", activity: undefined },
			{
				type: "chat/responsePart",
				turnId: "t2",
				part: {
					kind: "systemNotification",
					content: { markdown: "Context compacted · ~9k → ~2k tokens" },
				},
			},
			{ type: "chat/turnComplete", turnId: "t2", duration: 1 },
		],
		chat,
	);
	view = projection.project(reviewChatURI, reviewSession, chat, 3);
	assert.equal(view.activity, undefined);
	assert.equal(
		view.entries.at(-1)!.content,
		"Context compacted · ~9k → ~2k tokens",
	);
	assert.notEqual(
		view.entries[1].id,
		view.entries[3].id,
		"notice ids are unique per turn",
	);
});

test("loading older turns and truncation keep completed entries consistent", () => {
	const projection = new AHPSessionProjection();
	const turn = (id: string) => ({
		id,
		message: reviewUser(id),
		responseParts: [{ kind: "markdown", id: `${id}-m`, content: id }],
		usage: { inputTokens: 10, outputTokens: 1 },
		state: "complete",
	});
	let chat = reviewRun([], {
		resource: reviewChatURI,
		title: "Review",
		status: 1,
		modifiedAt: "",
		turns: [turn("t3"), turn("t4")],
	});
	const first = projection.project(reviewChatURI, reviewSession, chat, 1);
	chat = reviewRun(
		[{ type: "chat/turnsLoaded", turns: [turn("t1"), turn("t2"), turn("t3")] }],
		chat,
	);
	const loaded = projection.project(reviewChatURI, reviewSession, chat, 2);
	assert.deepEqual(
		loaded.entries
			.filter((entry) => entry.type === "user")
			.map((entry) => entry.content),
		["t1", "t2", "t3", "t4"],
		"an already known turn is not duplicated",
	);
	assert.equal(loaded.entries.at(-1), first.entries.at(-1));
	assert.equal(loaded.usage.inputTokens, 40);
	chat = reviewRun([{ type: "chat/truncated", turnId: "t2" }], chat);
	const truncated = projection.project(reviewChatURI, reviewSession, chat, 3);
	assert.deepEqual(
		truncated.entries
			.filter((entry) => entry.type === "user")
			.map((entry) => entry.content),
		["t1", "t2"],
	);
	assert.equal(truncated.usage.inputTokens, 20);
});

test("questions become ask prompts until answered", () => {
	const projection = new AHPSessionProjection();
	let chat = reviewRun([
		{
			type: "chat/turnStarted",
			turnId: "t1",
			startedAt: "2026-01-01T00:00:00Z",
			message: reviewUser("go"),
		},
		{
			type: "chat/inputRequested",
			request: {
				id: "r1",
				message: "Pick",
				questions: [
					{
						kind: "single-select",
						id: "color",
						title: "Color",
						required: true,
						options: [{ id: "red", label: "Red", recommended: true }],
					},
					{
						kind: "integer",
						id: "count",
						title: "Count",
						required: false,
						defaultValue: 2,
					},
				],
			},
		},
	]);
	const asked = projection.project(reviewChatURI, reviewSession, chat, 1);
	assert.equal(asked.prompts.length, 1);
	assert.deepEqual(
		asked.prompts[0].fields!.map((field) => [
			field.name,
			field.type,
			field.default,
			field.strict,
		]),
		[
			["color", "string", "red", true],
			["count", "integer", 2, undefined],
		],
	);
	chat = reviewRun(
		[
			{
				type: "chat/inputCompleted",
				requestId: "r1",
				response: "accept",
				answers: {},
			},
		],
		chat,
	);
	assert.deepEqual(
		projection.project(reviewChatURI, reviewSession, chat, 2).prompts,
		[],
	);
});

// Bug: queue positions are the 0-based index of the host's queue (TurnManager numbers them from 1), so the first follow-up is labelled "Queued", the second "#1", and a steering message sorted to the front keeps a later number; positions should follow the displayed order, numbering follow-ups from 1.
test("queued messages are numbered from one in the order they are shown", () => {
	const chat = reviewRun([
		{
			type: "chat/turnStarted",
			turnId: "t1",
			startedAt: "2026-01-01T00:00:00Z",
			message: reviewUser("work"),
		},
		{
			type: "chat/pendingMessageSet",
			kind: "queued",
			id: "q1",
			message: reviewUser("one"),
		},
		{
			type: "chat/pendingMessageSet",
			kind: "queued",
			id: "q2",
			message: reviewUser("two"),
		},
		{
			type: "chat/pendingMessageSet",
			kind: "steering",
			id: "s1",
			message: reviewUser("now"),
		},
	]);
	const inputs = new AHPSessionProjection().project(
		reviewChatURI,
		reviewSession,
		chat,
		1,
	).pendingInputs;
	assert.deepEqual(
		inputs.map((input) => input.id),
		["s1", "q1", "q2"],
	);
	const followUps = inputs.filter((input) => input.intent === "follow_up");
	assert.deepEqual(
		followUps.map((input) => input.position),
		[1, 2],
	);
	for (let i = 1; i < inputs.length; i++)
		assert.ok(
			inputs[i - 1].position <= inputs[i].position,
			`positions follow the displayed order: ${JSON.stringify(inputs.map((input) => [input.id, input.position]))}`,
		);
});

test("a turn the host started itself shows its response without a user message", () => {
	const projection = new AHPSessionProjection();
	const chat = reviewRun([], {
		resource: reviewChatURI,
		title: "Review",
		status: 1,
		modifiedAt: "",
		turns: [
			{
				id: "task",
				message: { text: "", origin: { kind: "systemNotification" } },
				responseParts: [
					{ kind: "markdown", id: "task-m", content: "Task finished" },
				],
				state: "complete",
			},
		],
	});
	const view = projection.project(reviewChatURI, reviewSession, chat, 1);
	assert.deepEqual(
		view.entries.map((entry) => [entry.type, entry.content]),
		[["assistant", "Task finished"]],
	);
});
