import { EditorConnection } from "./editorConnection.ts";
import type {
	DebugBreakpoint,
	DebugInspection,
	DebugLaunchPlan,
	DebugSession,
	DebugSourceBreakpoint,
	DebugState,
	DebugStackFrame,
	DebugScopeInspection,
	DebugEvaluation,
	DebugVariable,
} from "../api/debug.ts";

export class DAPClient {
	readonly connection: EditorConnection;
	private root = "";
	private session?: DebugSession;
	private output = "";
	// UTF-16 offset that follows `output` in the host's output stream.
	private outputEnd = 0;
	private outputSession?: string;
	private restored = false;
	private revision = 0;
	private listeners = new Set<(event: string) => void>();
	constructor(instance: string) {
		this.connection = new EditorConnection("dap", instance, {
			clientID: "wingman-web",
			adapterID: "wingman",
			linesStartAt1: true,
			columnsStartAt1: true,
			pathFormat: "path",
			supportsVariablePaging: true,
		});
		this.connection.onReady = (result) => {
			this.root =
				result._wingman.workspacePath.replaceAll("\\", "/").replace(/\/$/, "") +
				"/";
		};
		this.connection.onEvent((event, body) => {
			if (event === "disconnected") this.restored = false;
			if (event === "wingman/status") {
				this.revision++;
				this.resetOutput(body.session?.session_id);
				this.session = body.session ?? undefined;
			}
			if (event === "output") {
				if (body._wingmanSessionId) this.resetOutput(body._wingmanSessionId);
				this.appendOutput(body.output, body._wingmanOffset);
			}
			for (const listener of this.listeners) listener(event);
		});
	}
	private resetOutput(sessionID?: string) {
		if (this.outputSession === sessionID) return;
		this.output = "";
		this.outputEnd = 0;
		this.outputSession = sessionID;
	}
	// The host sends each output range once; a status snapshot can overlap
	// output events that were already in flight.
	private appendOutput(text: string, offset?: number) {
		if (offset === undefined) {
			this.output += text;
		} else {
			if (offset + text.length <= this.outputEnd) return;
			this.output += text.slice(Math.max(0, this.outputEnd - offset));
			this.outputEnd = offset + text.length;
		}
		if (this.output.length > 2 * 1024 * 1024)
			this.output = this.output.slice(-2 * 1024 * 1024);
	}
	private absolute(path: string) {
		return this.root + path.replace(/^\/+/, "");
	}
	private relative(path: string) {
		const normalized = path.replaceAll("\\", "/");
		// Windows paths compare case-insensitively, including the drive letter.
		const windows = /^[a-z]:\//i.test(this.root);
		const [candidate, root] = windows
			? [normalized.toLowerCase(), this.root.toLowerCase()]
			: [normalized, this.root];
		return root && candidate.startsWith(root)
			? normalized.slice(root.length)
			: "";
	}
	close() {
		this.connection.close();
	}
	subscribe(listener: (event: string) => void) {
		this.listeners.add(listener);
		return () => {
			this.listeners.delete(listener);
		};
	}
	private args(sessionId?: string, stateVersion?: number) {
		return { _wingmanSessionId: sessionId, _wingmanStateVersion: stateVersion };
	}
	private frame(frame: DebugStackFrame) {
		if (!frame.source?.path) return frame;
		return {
			...frame,
			source: { ...frame.source, path: this.relative(frame.source.path) },
		};
	}
	async status(path?: string, signal?: AbortSignal) {
		await this.connection.ready();
		const revision = this.revision;
		const result = await this.connection.request<{
			session?: DebugSession;
			output: string;
			_wingmanOutputOffset: number;
			breakpoints: DebugSourceBreakpoint[];
			resolved: DebugBreakpoint[];
		}>(
			"wingman/status",
			{ source: path ? { path: this.absolute(path) } : undefined },
			signal,
		);
		// Never overwrite a status event that overtook this snapshot response.
		if (revision === this.revision) this.session = result.session;
		const sessionID = result.session?.session_id;
		if (
			sessionID === this.session?.session_id &&
			(sessionID !== this.outputSession ||
				result._wingmanOutputOffset >= this.outputEnd)
		) {
			this.output = result.output;
			this.outputEnd = result._wingmanOutputOffset;
			this.outputSession = sessionID;
		}
		this.restored = true;
		return { ...result, session: this.session, output: this.output };
	}
	async launch(
		plan: DebugLaunchPlan,
		signal?: AbortSignal,
	): Promise<DebugSession> {
		const lifecycle = new AbortController();
		const requestSignal = signal
			? AbortSignal.any([signal, lifecycle.signal])
			: lifecycle.signal;
		let configuring: Promise<unknown> | undefined;
		const release = this.connection.onEvent((event) => {
			if (event === "initialized")
				configuring = this.connection
					.request("configurationDone", {}, requestSignal)
					.catch((error) => lifecycle.abort(error));
		});
		try {
			// The host launches its own reviewed copy; only the dialog's choices
			// travel with the id. Prelaunch builds can outlast any fixed timeout.
			await this.connection.request(
				plan.request,
				{ _wingmanPlanId: plan.id, _wingmanPlan: plan },
				requestSignal,
				{ timeout: 0 },
			);
			await configuring;
			requestSignal.throwIfAborted();
			const result = await this.status(undefined, requestSignal);
			if (!result.session)
				throw new Error("Debugger started without a session");
			return result.session;
		} finally {
			release();
			lifecycle.abort();
		}
	}
	async state(path?: string, signal?: AbortSignal): Promise<DebugState> {
		const status = await this.status(path, signal);
		const result: DebugState = {
			available: true,
			session: status.session,
			breakpoints: status.breakpoints,
			resolved: status.resolved,
		};
		if (status.session?.state === "stopped") {
			try {
				const frames = await this.stack(status.session, 1, signal);
				result.frame = frames[0];
			} catch (error) {
				result.frame_error = String(error);
			}
		}
		return result;
	}
	private async stack(
		session: DebugSession,
		levels: number,
		signal?: AbortSignal,
	) {
		const result = await this.connection.request<{
			stackFrames: DebugStackFrame[];
		}>(
			"stackTrace",
			{
				...this.args(session.session_id, session.state_version),
				threadId: session.stop?.thread_id ?? 0,
				startFrame: 0,
				levels,
			},
			signal,
		);
		return result.stackFrames.map((frame) => this.frame(frame));
	}
	async inspection(
		details: boolean,
		signal?: AbortSignal,
	): Promise<DebugInspection> {
		// Output arrives incrementally; refetching the whole buffer per event
		// is only needed before this connection has restored it.
		if (!details && this.restored)
			return {
				session: this.session,
				output: this.output,
				threads: [],
				frames: [],
			};
		const status = await this.status(undefined, signal);
		const result: DebugInspection = {
			session: status.session,
			output: status.output,
			threads: [],
			frames: [],
		};
		if (details && status.session?.state === "stopped") {
			try {
				const [threads, frames] = await Promise.all([
					this.connection.request<{ threads: DebugInspection["threads"] }>(
						"threads",
						this.args(status.session.session_id, status.session.state_version),
						signal,
					),
					this.stack(status.session, 100, signal),
				]);
				if (
					this.session?.state_version !== status.session.state_version ||
					this.session?.session_id !== status.session.session_id
				)
					return { ...result, session: this.session };
				result.threads = threads.threads;
				result.frames = frames;
			} catch (error) {
				result.error = String(error);
			}
		}
		return result;
	}
	async breakpoints(
		path: string,
		breakpoints: DebugSourceBreakpoint[],
		signal?: AbortSignal,
	) {
		await this.connection.ready();
		const result = await this.connection.request<any>(
			"setBreakpoints",
			{
				source: { path: this.absolute(path) },
				breakpoints: breakpoints.map((point) => ({
					line: point.line,
					column: point.column,
					condition: point.condition,
					hitCondition: point.hit_condition,
					logMessage: point.log_message,
				})),
			},
			signal,
		);
		return { breakpoints, resolved: result.breakpoints };
	}
	async control(
		operation: string,
		sessionId?: string,
		threadId?: number,
		signal?: AbortSignal,
	) {
		await this.connection.request(
			operation === "stop" ? "disconnect" : operation,
			{
				...this.args(sessionId),
				threadId: threadId ?? 0,
				...(operation === "stop" ? { terminateDebuggee: true } : {}),
			},
			signal,
		);
		return this.status(undefined, signal);
	}
	async evaluate(
		expression: string,
		sessionId?: string,
		frameId?: number,
		stateVersion?: number,
		signal?: AbortSignal,
	): Promise<DebugEvaluation> {
		return nativeValue(
			await this.connection.request(
				"evaluate",
				{
					...this.args(sessionId, stateVersion),
					expression,
					frameId,
					context: "hover",
				},
				signal,
			),
		);
	}
	async scopes(
		frameId: number,
		sessionId?: string,
		stateVersion?: number,
		signal?: AbortSignal,
	) {
		const result = await this.connection.request<{ scopes: any[] }>(
			"scopes",
			{ ...this.args(sessionId, stateVersion), frameId },
			signal,
		);
		const scopes: DebugScopeInspection[] = await Promise.all(
			result.scopes.map(async (wire) => {
				const scope = {
					name: wire.name,
					expensive: wire.expensive,
					...nativeValue(wire),
				};
				try {
					const result = wire.variablesReference
						? await this.variables(
								wire.variablesReference,
								sessionId,
								stateVersion,
								signal,
							)
						: { variables: [] };
					return { scope, variables: result.variables };
				} catch (error) {
					return { scope, variables: [], error: String(error) };
				}
			}),
		);
		return { scopes };
	}
	async variables(
		variablesReference: number,
		sessionId?: string,
		stateVersion?: number,
		signal?: AbortSignal,
	): Promise<{ variables: DebugVariable[] }> {
		const result = await this.connection.request<{ variables: any[] }>(
			"variables",
			{
				...this.args(sessionId, stateVersion),
				variablesReference,
				start: 0,
				count: 200,
			},
			signal,
		);
		return {
			variables: result.variables.map((variable) => ({
				...variable,
				...nativeValue(variable),
			})),
		};
	}
}
function nativeValue(value: any): any {
	return {
		...value,
		variables_reference: value.variablesReference,
		named_variables: value.namedVariables,
		indexed_variables: value.indexedVariables,
		evaluate_name: value.evaluateName,
	};
}
