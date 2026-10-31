import assert from "node:assert/strict";
import test from "node:test";
import {
	AHPSessionProjection,
	inputAnswers,
	toolLocation,
} from "../src/state/ahpSession.ts";

const chatURI = "ahp-chat:/wingman/b25l/c2F2ZWQ";
const root: any = {
	agents: [
		{
			provider: "one",
			displayName: "One",
			description: "",
			capabilities: { multipleChats: { fork: true } },
			models: [
				{
					id: "small",
					provider: "one",
					name: "Small",
					vendor: "anthropic",
					maxContextWindow: 1000,
					configSchema: {
						type: "object",
						properties: {
							effort: {
								type: "string",
								title: "Effort",
								enum: ["low", "high"],
								default: "low",
							},
						},
					},
				},
				{
					id: "large",
					provider: "one",
					name: "Large",
					maxContextWindow: 4000,
				},
			],
		},
	],
};
const schema = {
	type: "object",
	properties: {
		model: {
			type: "string",
			title: "Model",
			enum: ["small", "large"],
			enumLabels: ["Small", "Large"],
			chatMutable: true,
		},
		mode: {
			type: "string",
			title: "Mode",
			enum: ["build", "plan"],
			enumLabels: ["Build", "Plan"],
			enumDescriptions: ["Edits files", "Read only"],
			default: "build",
			chatMutable: true,
		},
		unattended: { type: "boolean", title: "Unattended", chatMutable: true },
		effort: { type: "string", title: "Effort", chatMutable: true },
	},
};
function session(values: Record<string, unknown> = {}): any {
	return {
		provider: "one",
		title: "Saved",
		status: 1,
		lifecycle: "ready",
		activeClients: [],
		chats: [{ resource: chatURI, title: "Saved", status: 1, modifiedAt: "" }],
		defaultChat: chatURI,
		config: { schema, values: { model: "small", ...values } },
	};
}
function chat(extra: Record<string, unknown> = {}): any {
	return {
		resource: chatURI,
		title: "Saved",
		status: 1,
		modifiedAt: "",
		turns: [],
		...extra,
	};
}
const user = (text: string) => ({ text, origin: { kind: "user" } });
const project = (s: any, c: any) =>
	new AHPSessionProjection("file:///work%20space").project(chatURI, s, c, 1, {
		uri: "ahp-session:/wingman/b25l/c2F2ZWQ",
		root,
	});

test("settings come from the chat's config, the schema and the root catalogue", () => {
	const view = project(
		session({ mode: "plan" }),
		chat({
			config: { values: { effort: "high", unattended: true, mode: "build" } },
		}),
	);
	assert.equal(view.session, "ahp-session:/wingman/b25l/c2F2ZWQ");
	assert.equal(view.provider, "one");
	assert.deepEqual(view.settings, {
		models: [
			{
				id: "small",
				name: "Small",
				namespace: "anthropic",
				efforts: ["low", "high"],
			},
			{ id: "large", name: "Large", namespace: undefined, efforts: [] },
		],
		model: "small",
		effort: "high",
		efforts: ["low", "high"],
		modes: [
			{ id: "build", name: "Build", description: "Edits files" },
			{ id: "plan", name: "Plan", description: "Read only" },
		],
		// A chat's own values win over the session's default-chat mirror.
		mode: "build",
		unattended: true,
		canFork: true,
	});
	assert.equal(
		project(session({ mode: "plan" }), chat()).settings.mode,
		"plan",
	);
});

test("phase, activity, usage and errors are derived from chat state", () => {
	const active = (parts: any[]) =>
		chat({
			activity: "Retrying · overloaded · attempt 2 in 3s",
			turns: [
				{
					id: "t1",
					message: user("first"),
					responseParts: [],
					usage: {
						inputTokens: 100,
						outputTokens: 10,
						cacheReadTokens: 50,
						model: "small",
					},
					state: "complete",
				},
			],
			activeTurn: {
				id: "t2",
				startedAt: "",
				message: user("second"),
				responseParts: parts,
				usage: { inputTokens: 300, outputTokens: 5, model: "small" },
			},
		});
	assert.equal(project(session(), active([])).phase, "thinking");
	assert.equal(
		project(session(), active([{ kind: "reasoning", id: "r", content: "" }]))
			.phase,
		"thinking",
	);
	assert.equal(
		project(session(), active([{ kind: "markdown", id: "m", content: "" }]))
			.phase,
		"streaming",
	);
	const tool = {
		kind: "toolCall",
		toolCall: {
			toolCallId: "c",
			toolName: "read",
			displayName: "Read",
			status: "streaming",
		},
	};
	const view = project(session(), active([tool]));
	assert.equal(view.phase, "tool_running");
	assert.equal(view.activity, "Retrying · overloaded · attempt 2 in 3s");
	assert.deepEqual(view.usage, {
		inputTokens: 400,
		outputTokens: 15,
		cachedTokens: 50,
		lastInputTokens: 300,
		contextWindow: 1000,
	});
	assert.equal(project(session(), chat()).phase, "idle");
	assert.equal(project(session(), chat()).activity, undefined);
	const failed = chat({
		turns: [
			{
				id: "t1",
				message: user("x"),
				responseParts: [
					{
						kind: "error",
						error: { errorType: "x", message: "Model overloaded" },
					},
				],
				usage: undefined,
				state: "error",
			},
		],
	});
	assert.equal(project(session(), failed).error, "Model overloaded");
	assert.equal(project(session(), chat()).error, null);
});

test("entries take turn and part identities, and notifications become notices", () => {
	const view = project(
		session(),
		chat({
			turns: [
				{
					id: "t1",
					message: {
						...user("look"),
						attachments: [
							{
								type: "resource",
								label: "a.go",
								uri: "file:///work%20space/a.go",
							},
							{
								type: "embeddedResource",
								label: "Image",
								contentType: "image/png",
								data: "AA==",
							},
						],
					},
					responseParts: [
						{ kind: "reasoning", id: "r1", content: "hmm", duration: 1200 },
						{ kind: "systemNotification", content: "and also check b.go" },
						{
							kind: "toolCall",
							toolCall: {
								toolCallId: "call-1",
								toolName: "read",
								displayName: "Read",
								intention: "a.go",
								invocationMessage: "Reading a.go",
								toolInput: '{"path":"a.go"}',
								status: "completed",
								confirmed: "not-needed",
								success: true,
								pastTenseMessage: "Read a.go",
								_meta: { toolKind: "terminal" },
								content: [
									{ type: "text", text: "package a" },
									{ type: "resource", uri: "file:///work%20space/a.go#L12" },
									{ type: "resource", uri: "file:///elsewhere/b.go" },
									{
										type: "embeddedResource",
										contentType: "image/png",
										data: "AA==",
									},
								],
							},
						},
						{ kind: "markdown", id: "m1", content: "done" },
					],
					usage: undefined,
					state: "complete",
				},
			],
		}),
	);
	assert.deepEqual(
		view.entries.map((entry) => [entry.id, entry.type]),
		[
			["user:t1", "user"],
			["r1", "reasoning"],
			["t1:1", "notice"],
			["call-1", "tool"],
			["m1", "assistant"],
		],
	);
	assert.deepEqual(view.entries[0].files, ["a.go"]);
	assert.deepEqual(view.entries[0].images, ["data:image/png;base64,AA=="]);
	assert.equal(view.entries[0].inputId, "t1");
	assert.equal(view.entries[2].content, "and also check b.go");
	const tool = view.entries[3];
	assert.equal(tool.toolKind, "execute");
	assert.equal(tool.toolHint, "a.go");
	assert.equal(tool.toolArgs, '{"path":"a.go"}');
	assert.equal(tool.toolResult, "package a");
	assert.deepEqual(tool.toolLocations, [
		{ path: "a.go", line: 12 },
		{ path: "/elsewhere/b.go" },
	]);
	assert.deepEqual(tool.images, ["data:image/png;base64,AA=="]);
	assert.deepEqual(toolLocation("not a uri", ""), undefined);
});

test("confirmations carry their options and questions their defaults", () => {
	const view = project(
		session(),
		chat({
			activeTurn: {
				id: "t1",
				startedAt: "",
				message: user("go"),
				usage: undefined,
				responseParts: [
					{
						kind: "toolCall",
						toolCall: {
							toolCallId: "approval:7",
							toolName: "bash",
							displayName: "Bash",
							invocationMessage: "Run rm -rf build",
							status: "pending-confirmation",
							options: [
								{ id: "once", label: "Allow", kind: "approve" },
								{
									id: "session",
									label: "Allow for this session",
									kind: "approve",
									group: 1,
								},
								{ id: "deny", label: "Deny", kind: "deny" },
							],
						},
					},
					{
						kind: "inputRequest",
						request: {
							id: "q",
							message: "Pick",
							questions: [
								{
									id: "name",
									kind: "text",
									message: "Name",
									defaultValue: "x",
								},
								{
									id: "count",
									kind: "integer",
									message: "Count",
									defaultValue: 2,
								},
								{
									id: "ok",
									kind: "boolean",
									message: "OK?",
									defaultValue: true,
								},
								{
									id: "color",
									kind: "single-select",
									message: "Color",
									allowFreeformInput: true,
									options: [
										{ id: "red", label: "Red" },
										{
											id: "blue",
											label: "Blue",
											description: "Calm",
											recommended: true,
										},
									],
								},
								{
									id: "tags",
									kind: "multi-select",
									message: "Tags",
									options: [
										{ id: "a", label: "A", recommended: true },
										{ id: "b", label: "B", recommended: true },
									],
								},
							],
						},
					},
				],
			},
		}),
	);
	assert.equal(view.entries.length, 1);
	assert.deepEqual(view.prompts[0], {
		id: "approval:7",
		kind: "confirm",
		message: "Run rm -rf build",
		options: [
			{ id: "once", label: "Allow", kind: "approve" },
			{
				id: "session",
				label: "Allow for this session",
				kind: "approve",
				group: 1,
			},
			{ id: "deny", label: "Deny", kind: "deny" },
		],
	});
	const fields = view.prompts[1].fields!;
	assert.deepEqual(
		fields.map((field) => [field.name, field.type, field.default]),
		[
			["name", "string", "x"],
			["count", "integer", 2],
			["ok", "boolean", true],
			["color", "string", "blue"],
			["tags", "string", ["a", "b"]],
		],
	);
	assert.deepEqual(fields[3].enum_labels, ["Red", "Blue"]);
	assert.deepEqual(fields[3].enum_descriptions, ["", "Calm"]);
	assert.equal(fields[3].strict, false);
	assert.equal(fields[4].multiple, true);
});

test("answers map free-form select values and numeric kinds", () => {
	const state = chat({
		activeTurn: {
			id: "t1",
			startedAt: "",
			message: user("go"),
			usage: undefined,
			responseParts: [
				{
					kind: "inputRequest",
					request: {
						id: "q",
						questions: [
							{ id: "count", kind: "integer", message: "" },
							{
								id: "color",
								kind: "single-select",
								message: "",
								options: [{ id: "red", label: "Red" }],
							},
							{
								id: "tags",
								kind: "multi-select",
								message: "",
								options: [{ id: "a", label: "A" }],
							},
						],
					},
				},
			],
		},
	});
	assert.deepEqual(
		{
			...inputAnswers(state, "q", {
				count: "3",
				color: "green",
				tags: ["a", "c"],
			}),
		},
		{
			count: { state: "submitted", value: { kind: "number", value: 3 } },
			color: {
				state: "submitted",
				value: { kind: "selected", value: "", freeformValues: ["green"] },
			},
			tags: {
				state: "submitted",
				value: { kind: "selected-many", value: ["a"], freeformValues: ["c"] },
			},
		},
	);
});
