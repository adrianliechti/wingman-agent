import type {
	SessionAddedParams,
	SessionRemovedParams,
	SessionSummary,
	SessionSummaryChangedParams,
} from "@microsoft/agent-host-protocol";
import type { AHPConnection } from "./ahpConnection.ts";

export type CatalogState = {
	sessions: readonly SessionSummary[];
	/** The host listed its sessions at least once on this connection. */
	loaded: boolean;
	error: string | null;
	/** Providers still starting, whose own sessions are not listed yet. */
	starting: readonly string[];
};

const EMPTY: CatalogState = {
	sessions: [],
	loaded: false,
	error: null,
	starting: [],
};

/** The host's session list: one `listSessions`, then the root notifications
 * keep it current, so a busy session never refetches the whole list. It starts
 * with its first subscriber and reloads after every reconnect. */
export class SessionCatalog {
	private sessions = new Map<string, SessionSummary>();
	private state: CatalogState = EMPTY;
	private listeners = new Set<() => void>();
	private started = false;
	private loading?: Promise<void>;
	/** A list requested while one runs, which starts after it. */
	private queued?: Promise<void>;
	/** Notifications that arrive while a list is in flight apply on top of it. */
	private buffered: { method: string; params: unknown }[] | undefined;
	private reloadTimer?: ReturnType<typeof setTimeout>;
	/** Providers started on this connection so that they list their sessions. */
	private included = new Map<string, Promise<unknown>>();
	/** Providers to start again on every connection. */
	private wanted = new Set<string>();
	private readonly ahp: AHPConnection;
	private readonly onSummary: (summary: SessionSummary) => void;
	constructor(
		ahp: AHPConnection,
		onSummary: (summary: SessionSummary) => void = () => {},
	) {
		this.ahp = ahp;
		this.onSummary = onSummary;
	}
	readonly getSnapshot = () => this.state;
	readonly subscribe = (listener: () => void) => {
		this.listeners.add(listener);
		this.start();
		return () => {
			this.listeners.delete(listener);
		};
	};
	private start() {
		if (this.started) return;
		this.started = true;
		this.ahp.onRootNotification((method, params) =>
			this.notify(method, params),
		);
		this.ahp.subscribeConnection(() => {
			if (!this.ahp.getConnected()) {
				this.included.clear();
				this.publish({ starting: [] });
				return;
			}
			void this.reload().catch(() => {});
			for (const provider of this.wanted)
				void this.include(provider).catch(() => {});
		});
		if (this.ahp.getConnected()) void this.reload().catch(() => {});
	}
	/** Lists the sessions again. Callers during a list share the next one, which
	 * starts after it, so that what they changed before calling is listed. */
	reload(): Promise<void> {
		if (!this.loading) return this.read();
		this.queued ??= this.loading
			.catch(() => {})
			.then(() => {
				this.queued = undefined;
				return this.read();
			});
		return this.queued;
	}
	/** Readers can share the list already in flight. */
	read(): Promise<void> {
		this.loading ??= this.load().finally(() => {
			this.loading = undefined;
		});
		return this.loading;
	}
	private async load() {
		this.buffered = [];
		const buffered = this.buffered;
		let change: Partial<CatalogState> = { loaded: true, error: null };
		try {
			const result = await this.ahp.request("listSessions", {
				channel: "ahp-root://",
			});
			this.sessions = new Map(
				result.items.map((summary) => [summary.resource, summary]),
			);
			for (const summary of result.items) this.onSummary(summary);
		} catch (error) {
			change = { error: String(error) };
			throw error;
		} finally {
			this.buffered = undefined;
			for (const { method, params } of buffered) this.apply(method, params);
			this.publish(change);
		}
	}
	/** Starts a provider so that its own sessions, such as those of its CLI, are listed too. */
	include(provider: string): Promise<unknown> {
		this.wanted.add(provider);
		let started = this.included.get(provider);
		if (!started) {
			this.publish({ starting: [...this.state.starting, provider] });
			started = this.ahp
				.request("resolveSessionConfig", {
					channel: "ahp-root://",
					provider,
					// It starts in the window's project, whose own sessions it then
					// lists; an agent that lists every folder's runs once for the host.
					...(this.ahp.directory
						? { workingDirectory: this.ahp.directory }
						: {}),
					_meta: { "wingman.listing": true },
				})
				.then(() => {
					if (this.included.get(provider) === started) return this.reload();
				})
				.finally(() => {
					if (this.included.get(provider) === started)
						this.publish({
							starting: this.state.starting.filter((id) => id !== provider),
						});
				})
				.catch((error) => {
					if (this.included.get(provider) === started)
						this.included.delete(provider);
					throw error;
				});
			this.included.set(provider, started);
		}
		return started;
	}
	private notify(method: string, params: unknown) {
		if (this.buffered) {
			this.buffered.push({ method, params });
			return;
		}
		this.apply(method, params);
		this.publish({});
	}
	private apply(method: string, params: unknown) {
		if (method === "root/sessionAdded") {
			const { summary } = params as SessionAddedParams;
			this.sessions.set(summary.resource, summary);
			this.onSummary(summary);
		} else if (method === "root/sessionRemoved") {
			this.sessions.delete((params as SessionRemovedParams).session);
		} else if (method === "root/sessionSummaryChanged") {
			const { session, changes } = params as SessionSummaryChangedParams;
			const current = this.sessions.get(session);
			// A session the list never named is listed again rather than guessed.
			if (!current) {
				this.scheduleReload();
				return;
			}
			const next = { ...current, ...changes } as SessionSummary;
			this.sessions.set(session, next);
			this.onSummary(next);
		}
	}
	private scheduleReload() {
		this.reloadTimer ??= setTimeout(() => {
			this.reloadTimer = undefined;
			void this.reload().catch(() => {});
		}, 250);
	}
	/** The summary of a listed session. */
	summary(session: string): SessionSummary | undefined {
		return this.sessions.get(session);
	}
	/** Removes a session the client disposed before the host confirms it. */
	forget(session: string) {
		if (!this.sessions.delete(session)) return;
		this.publish({});
	}
	private publish(change: Partial<CatalogState>) {
		this.state = {
			...this.state,
			...change,
			sessions: [...this.sessions.values()],
		};
		for (const listener of this.listeners) listener();
	}
}
