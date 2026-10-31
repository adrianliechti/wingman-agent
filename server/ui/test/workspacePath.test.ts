import assert from "node:assert/strict";
import test from "node:test";
import {
	isAbsolutePath,
	workspaceFileTarget,
} from "../src/utils/workspacePath.ts";

test("maps absolute paths inside the workspace to relative paths", () => {
	assert.deepEqual(
		workspaceFileTarget("/work/demo/server/main.go", "/work/demo/"),
		{
			path: "server/main.go",
			external: false,
		},
	);
	assert.deepEqual(workspaceFileTarget("/work/demo", "/work/demo"), {
		path: "",
		external: false,
	});
});

test("keeps sibling and outside paths external", () => {
	assert.deepEqual(workspaceFileTarget("/work/demo-2/main.go", "/work/demo"), {
		path: "/work/demo-2/main.go",
		external: true,
	});
});

test("compares Windows drive paths case-insensitively", () => {
	assert.deepEqual(
		workspaceFileTarget("c:\\Work\\Demo\\src\\app.ts", "C:/work/demo"),
		{
			path: "src/app.ts",
			external: false,
		},
	);
});

test("resolves dot segments and keeps escaping paths external", () => {
	assert.deepEqual(
		workspaceFileTarget("/work/demo/src/../x.go", "/work/demo"),
		{
			path: "x.go",
			external: false,
		},
	);
	assert.deepEqual(
		workspaceFileTarget("/work/demo/../other/x.go", "/work/demo"),
		{ path: "/work/demo/../other/x.go", external: true },
	);
});

test("recognizes absolute paths", () => {
	assert.equal(isAbsolutePath("/etc/hosts"), true);
	assert.equal(isAbsolutePath("C:\\work"), true);
	assert.equal(isAbsolutePath("src/main.go"), false);
});
