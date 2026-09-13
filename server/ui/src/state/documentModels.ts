import type { editor } from "monaco-editor";

type OwnedModel = {
	uri: string;
	model: editor.ITextModel | null;
	view: editor.ICodeEditorViewState | null;
	users: number;
	closed: boolean;
};
// Mounting owns an editor; opening a document owns its model and undo stack.
// This module uses only types from Monaco, so document state stays lightweight.
const models = new Map<string, OwnedModel>();
function documentModel(path: string): OwnedModel {
	let entry = models.get(path);
	if (!entry) {
		entry = {
			// Paths can be reused while a renamed or closing model still exists.
			uri: `wingman-document:/${crypto.randomUUID()}/${encodeURIComponent(path)}`,
			model: null,
			view: null,
			users: 0,
			closed: false,
		};
		models.set(path, entry);
	}
	return entry;
}
export function documentModelURI(path: string) {
	return documentModel(path).uri;
}
export function retainDocumentModel(path: string, model: editor.ITextModel) {
	const entry = documentModel(path);
	entry.model = model;
	entry.users++;
	let released = false;
	return {
		view: entry.view,
		release(view: editor.ICodeEditorViewState | null) {
			if (released) return;
			released = true;
			entry.view = view;
			entry.users--;
			if (entry.closed && !entry.users) entry.model?.dispose();
		},
	};
}
export function closeDocumentModel(path: string) {
	const entry = models.get(path);
	if (!entry) return;
	models.delete(path);
	entry.closed = true;
	if (!entry.users) entry.model?.dispose();
}
export function moveDocumentModels(from: string, to: string) {
	if (from === to) return;
	for (const [path, entry] of [...models]) {
		if (path !== from && !path.startsWith(`${from}/`)) continue;
		const target = `${to}${path.slice(from.length)}`;
		models.delete(path);
		closeDocumentModel(target);
		models.set(target, entry);
	}
}
