import assert from "node:assert/strict";
import test from "node:test";
import type { editor } from "monaco-editor";
import {
	retainDocumentModel,
	closeDocumentModel,
	documentModelURI,
	moveDocumentModels,
} from "../src/state/documentModels.ts";

function modelFor(path: string) {
	const uri = documentModelURI(path);
	let disposals = 0;
	return {
		uri,
		model: {
			dispose: () => disposals++,
			uri: { toString: () => uri },
		} as unknown as editor.ITextModel,
		get disposals() {
			return disposals;
		},
	};
}

test("open documents retain model and view state until actual close", () => {
	const owned = modelFor("file");
	assert.equal(documentModelURI("file"), owned.uri);
	const view = { scrollTop: 72 } as unknown as editor.ICodeEditorViewState;
	const first = retainDocumentModel("file", owned.model);
	first.release(view);
	assert.equal(owned.disposals, 0);
	const second = retainDocumentModel("file", owned.model);
	assert.equal(second.view, view);
	closeDocumentModel("file");
	assert.equal(owned.disposals, 0);
	second.release(view);
	assert.equal(owned.disposals, 1);
});

test("folder rename keeps the model URI until its last editor closes", () => {
	const owned = modelFor("old/file");
	const first = retainDocumentModel("old/file", owned.model);
	moveDocumentModels("old", "new");
	assert.equal(documentModelURI("new/file"), owned.uri);
	const second = retainDocumentModel("new/file", owned.model);
	closeDocumentModel("new/file");
	first.release(null);
	assert.equal(owned.disposals, 0);
	second.release(null);
	assert.equal(owned.disposals, 1);
});

test("reusing a renamed file's old path gets an independent editor model", () => {
	const owned = modelFor("reused.txt");
	const retained = retainDocumentModel("reused.txt", owned.model);
	moveDocumentModels("reused.txt", "renamed.txt");
	assert.equal(documentModelURI("renamed.txt"), owned.uri);
	assert.notEqual(documentModelURI("reused.txt"), owned.uri);
	retained.release(null);
	assert.equal(documentModelURI("renamed.txt"), owned.uri);
	closeDocumentModel("renamed.txt");
	closeDocumentModel("reused.txt");
	assert.equal(owned.disposals, 1);
});

test("consecutive renames do not leave stale model aliases", () => {
	const owned = modelFor("chain/start.txt");
	const retained = retainDocumentModel("chain/start.txt", owned.model);
	moveDocumentModels("chain/start.txt", "chain/middle.txt");
	moveDocumentModels("chain/middle.txt", "chain/end.txt");
	moveDocumentModels("chain", "moved");
	assert.equal(documentModelURI("moved/end.txt"), owned.uri);
	retained.release(null);
	closeDocumentModel("moved/end.txt");
	assert.equal(owned.disposals, 1);
	for (const path of [
		"chain/start.txt",
		"chain/middle.txt",
		"moved/middle.txt",
	]) {
		assert.notEqual(documentModelURI(path), owned.uri);
		closeDocumentModel(path);
	}
});

test("closing and reopening a path before editor cleanup creates a new model", () => {
	const old = modelFor("reopened.txt");
	const oldEditor = retainDocumentModel("reopened.txt", old.model);
	closeDocumentModel("reopened.txt");
	const reopened = modelFor("reopened.txt");
	assert.notEqual(reopened.uri, old.uri);
	const newEditor = retainDocumentModel("reopened.txt", reopened.model);
	oldEditor.release(null);
	assert.equal(old.disposals, 1);
	assert.equal(reopened.disposals, 0);
	assert.equal(documentModelURI("reopened.txt"), reopened.uri);
	newEditor.release(null);
	closeDocumentModel("reopened.txt");
	assert.equal(reopened.disposals, 1);
});

test("repeated cleanup cannot release another editor's ownership", () => {
	const owned = modelFor("shared.txt");
	const first = retainDocumentModel("shared.txt", owned.model);
	const second = retainDocumentModel("shared.txt", owned.model);
	closeDocumentModel("shared.txt");
	closeDocumentModel("shared.txt");
	first.release(null);
	first.release(null);
	assert.equal(owned.disposals, 0);
	second.release(null);
	second.release(null);
	assert.equal(owned.disposals, 1);
});
