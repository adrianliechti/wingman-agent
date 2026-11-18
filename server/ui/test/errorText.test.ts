import assert from "node:assert/strict";
import test from "node:test";
import { RpcError } from "@microsoft/agent-host-protocol/client";
import { errorText } from "../src/utils/errorText.ts";

test("errors read as the words of whoever failed", () => {
	const passedOn = new RpcError(
		-32603,
		JSON.stringify({
			code: -32603,
			message: "Internal error",
			data: { error: "no on-disk session 0cde for cwd /tmp/project" },
		}),
	);
	assert.equal(
		errorText(passedOn),
		"no on-disk session 0cde for cwd /tmp/project",
	);
	assert.equal(
		errorText(new RpcError(-32001, "unknown session")),
		"unknown session",
	);
	assert.equal(errorText(new Error("Connection lost")), "Connection lost");
	assert.equal(errorText("plain"), "plain");
	assert.equal(errorText(new Error("{not json")), "{not json");
});
