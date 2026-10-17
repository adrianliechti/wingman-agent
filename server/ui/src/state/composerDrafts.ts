import { ComposerDraft, type ComposerContent } from "./composerDraft.ts";
import type { CenterTab } from "../mainLayout.ts";

export type SavedDraft = {
	tab: CenterTab;
	content: ComposerContent;
	open: boolean;
	updated: number;
};
export interface DraftStorage {
	read(): Promise<SavedDraft[]>;
	write(value: SavedDraft): Promise<void>;
	recover?(): SavedDraft[];
	protect?(pending: SavedDraft[]): void;
	clearRecovery?(): void;
}
const hasContent = (c: ComposerContent) =>
	!!(
		c.text ||
		c.files.length ||
		c.images.length ||
		c.previousDraft?.text ||
		c.previousDraft?.files.length ||
		c.previousDraft?.images.length
	);
const contentOf = (draft: ComposerDraft): ComposerContent => {
	const { text, files, images, editingQueueId, previousDraft } =
		draft.getSnapshot();
	return {
		text,
		files,
		images,
		editingQueueId,
		...(previousDraft ? { previousDraft } : {}),
	};
};
const nextRevision = (record?: SavedDraft) =>
	Math.max(Date.now(), (record?.updated ?? 0) + 1);

function isDraftContent(value: unknown): boolean {
	const content = value as Partial<ComposerContent> | null;
	return (
		typeof content?.text === "string" &&
		Array.isArray(content.files) &&
		content.files.every((path) => typeof path === "string") &&
		Array.isArray(content.images) &&
		content.images.every(
			(image) =>
				typeof image?.id === "string" &&
				typeof image.dataUrl === "string" &&
				(image.name === undefined || typeof image.name === "string"),
		)
	);
}

function isSavedDraft(value: unknown): value is SavedDraft {
	const record = value as Partial<SavedDraft> | null;
	return (
		record?.tab?.type === "chat" &&
		typeof record.tab.id === "string" &&
		!!record.tab.id &&
		typeof record.tab.label === "string" &&
		typeof record.open === "boolean" &&
		typeof record.updated === "number" &&
		Number.isSafeInteger(record.updated) &&
		isDraftContent(record.content) &&
		(record.content?.editingQueueId == null ||
			typeof record.content.editingQueueId === "string") &&
		(record.content?.previousDraft === undefined ||
			isDraftContent(record.content.previousDraft))
	);
}

// Draft ownership outlives mounted panels and closed tabs. IndexedDB retains
// attachments as well as text, without a synchronous multi-megabyte write on
// every keystroke. One ordered writer prevents late saves resurrecting sends.
export class ComposerDrafts {
	private records = new Map<string, SavedDraft>();
	private drafts = new Map<string, ComposerDraft>();
	private writes = new Map<string, SavedDraft>();
	private writer: Promise<void> | null = null;
	private listeners = new Set<() => void>();
	private error: string | null = null;
	private storage: DraftStorage;
	constructor(storage: DraftStorage) {
		this.storage = storage;
	}
	async load() {
		// Read the two stores independently: the navigation journal is most
		// useful when IndexedDB itself is unavailable. One bad row is isolated.
		for (const recover of [false, true]) {
			try {
				const records = recover
					? (this.storage.recover?.() ?? [])
					: await this.storage.read();
				for (const record of records) {
					if (!isSavedDraft(record)) continue;
					const saved = this.records.get(record.tab.id);
					if (saved && saved.updated >= record.updated) continue;
					this.records.set(record.tab.id, record);
					if (recover) this.writes.set(record.tab.id, record);
				}
			} catch {
				this.setError(
					"Draft recovery is unavailable. Keep this window open until your message is sent.",
				);
			}
		}
		if (this.writes.size) void this.flush();
	}
	readonly getError = () => this.error;
	readonly subscribe = (listener: () => void) => {
		this.listeners.add(listener);
		return () => {
			this.listeners.delete(listener);
		};
	};
	private setError(error: string | null) {
		if (this.error === error) return;
		this.error = error;
		for (const listener of this.listeners) listener();
	}
	get(tab: CenterTab): ComposerDraft {
		const existing = this.drafts.get(tab.id);
		if (existing) return existing;
		let record = this.records.get(tab.id);
		let tombstone: SavedDraft | undefined;
		if (!record && tab.sessionId) {
			const previous = [...this.records.values()]
				.filter(
					(r) =>
						r.tab.sessionId === tab.sessionId &&
						!r.open &&
						hasContent(r.content),
				)
				.sort((a, b) => b.updated - a.updated)[0];
			if (previous) {
				record = {
					...previous,
					tab,
					open: true,
					updated: nextRevision(previous),
				};
				// Keep a tombstone for the old identity so a reload cannot recover
				// both copies after this composer is sent.
				const empty = {
					...previous,
					content: contentOf(new ComposerDraft()),
					open: false,
					updated: nextRevision(previous),
				};
				this.records.set(previous.tab.id, empty);
				tombstone = empty;
			}
		}
		const draft = new ComposerDraft(record?.content);
		this.drafts.set(tab.id, draft);
		this.records.set(tab.id, {
			tab,
			open: true,
			content: contentOf(draft),
			updated: record?.open ? record.updated : nextRevision(record),
		});
		// Save the recovered identity before retiring the old one. A reload
		// between writes may recover two copies, but must never lose both.
		if (record && (tombstone || !record.open))
			this.enqueue(this.records.get(tab.id)!);
		if (tombstone) this.enqueue(tombstone);
		let previous = contentOf(draft);
		draft.subscribe(() => {
			const content = contentOf(draft);

			if (
				content.text === previous.text &&
				content.files === previous.files &&
				content.images === previous.images &&
				content.previousDraft === previous.previousDraft &&
				content.editingQueueId === previous.editingQueueId
			)
				return;
			previous = content;
			const next = {
				...this.records.get(tab.id)!,
				content,
				updated: nextRevision(this.records.get(tab.id)),
			};
			this.records.set(tab.id, next);
			this.enqueue(next);
		});
		return draft;
	}

	syncTabs(tabs: CenterTab[]) {
		for (const tab of tabs) if (tab.type === "chat") this.get(tab);
		for (const [id, record] of this.records) {
			const tab = tabs.find((t) => t.id === id);
			const open = !!tab;
			if (
				record.open === open &&
				(!tab || JSON.stringify(tab) === JSON.stringify(record.tab))
			)
				continue;
			const next = {
				...record,
				tab: tab ?? record.tab,
				open,
				updated: nextRevision(record),
			};
			this.records.set(id, next);
			if (hasContent(record.content)) this.enqueue(next);
		}
	}
	openTabs() {
		return [...this.records.values()]
			.filter((r) => r.open && hasContent(r.content))
			.map((r) => r.tab);
	}
	closedTabs() {
		return [...this.records.values()]
			.filter((r) => !r.open && hasContent(r.content))
			.sort((a, b) => b.updated - a.updated)
			.map((r) => ({
				tab: r.tab,
				preview: r.content.text.slice(0, 60) || "Attachments",
			}));
	}
	private enqueue(record: SavedDraft) {
		this.writes.set(record.tab.id, record);
		void this.flush();
	}
	// IndexedDB transactions can finish after navigation, but writes waiting
	// in JavaScript cannot. Journal the latest pending values synchronously at
	// the navigation boundary, including attachments and queue-edit backups.
	protectPending(): boolean {
		if (!this.writes.size) return true;
		if (!this.storage.protect) return false;
		try {
			this.storage.protect([...this.writes.values()]);
			return true;
		} catch {
			return false;
		}
	}
	readonly needsProtection = () => !!this.writer || this.writes.size > 0;
	async flush(): Promise<boolean> {
		if (this.writer) {
			await this.writer;
			return !this.writes.size;
		}
		if (!this.writes.size) return true;
		this.writer = (async () => {
			while (this.writes.size) {
				const [id, value] = this.writes.entries().next().value!;
				try {
					await this.storage.write(value);
					if (this.writes.get(id) === value) this.writes.delete(id);
				} catch {
					this.setError(
						"Your draft could not be saved on this device. Keep this window open, or retry saving before closing it.",
					);
					return;
				}
			}
			try {
				this.storage.clearRecovery?.();
			} catch {
				/* A retained journal is harmless; newer revisions win on recovery. */
			}
			this.setError(null);
		})();
		await this.writer;
		this.writer = null;
		// An enqueue can arrive after the draining promise resolves but before
		// this continuation releases it. Drain that write too, unless saving failed.
		if (this.writes.size && !this.error) return this.flush();
		return !this.writes.size;
	}
}

let registry: ComposerDrafts;
export const composerDrafts = () => registry;
export async function initializeComposerDrafts(workspace: string) {
	let database: IDBDatabase;
	const recoveryPrefix = `wingman-pending-drafts:${workspace}:`;
	const recoveryKey = recoveryPrefix + crypto.randomUUID();
	const recoveredJournals = new Map<string, string>();
	const ready = new Promise<IDBDatabase>((resolve, reject) => {
		const request = indexedDB.open("wingman-drafts", 1);
		request.onupgradeneeded = () =>
			request.result.createObjectStore("drafts", { keyPath: "key" });
		request.onsuccess = () => resolve(request.result);
		request.onerror = () => reject(request.error);
		request.onblocked = () => reject(new Error("Draft storage is busy"));
	});
	registry = new ComposerDrafts({
		recover() {
			const records: SavedDraft[] = [];
			for (let index = 0; index < localStorage.length; index++) {
				const key = localStorage.key(index);
				if (!key?.startsWith(recoveryPrefix)) continue;
				const raw = localStorage.getItem(key);
				if (raw === null) continue;
				try {
					const value: unknown = JSON.parse(raw);
					if (Array.isArray(value)) records.push(...value.filter(isSavedDraft));
					recoveredJournals.set(key, raw);
				} catch {
					// A damaged journal must not hide drafts in other journals.
				}
			}
			return records;
		},
		protect(pending) {
			localStorage.setItem(recoveryKey, JSON.stringify(pending));
		},
		clearRecovery() {
			localStorage.removeItem(recoveryKey);
			for (const [key, recovered] of recoveredJournals) {
				// Another window may have added new pending work to this journal.
				if (localStorage.getItem(key) === recovered)
					localStorage.removeItem(key);
			}
			recoveredJournals.clear();
		},
		async read() {
			database = await ready;
			return new Promise((resolve, reject) => {
				const request = database
					.transaction("drafts")
					.objectStore("drafts")
					.getAll();
				request.onsuccess = () =>
					resolve(
						request.result
							.filter((r) => r.workspace === workspace)
							.map((r) => r.value),
					);
				request.onerror = () => reject(request.error);
			});
		},
		async write(value) {
			database = await ready;
			return new Promise<void>((resolve, reject) => {
				const tx = database.transaction("drafts", "readwrite");
				tx.objectStore("drafts").put({
					key: JSON.stringify([workspace, value.tab.id]),
					workspace,
					value,
				});
				tx.oncomplete = () => resolve();
				tx.onabort = tx.onerror = () => reject(tx.error);
			});
		},
	});
	await registry.load();
	window.addEventListener("pagehide", () => {
		if (registry.needsProtection()) registry.protectPending();
	});
	window.addEventListener("beforeunload", (event) => {
		if (registry.needsProtection() && !registry.protectPending()) {
			event.preventDefault();
			event.returnValue = "";
		}
	});
}
