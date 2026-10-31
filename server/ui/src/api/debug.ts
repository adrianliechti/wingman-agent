import { workspaceClient } from "../state/workspaceClient.ts";
import { fetchJSON, fetchOK } from "./http.ts";

export type DebugAction = "run" | "debug";

export interface DebugTarget {
	id: string;
	name: string;
	detail?: string;
	kind: string;
	language: string;
	path: string;
	directory: string;
	line: number;
	column: number;
}

export interface DebugStop {
	reason: string;
	description?: string;
	thread_id?: number;
	all_threads_stopped?: boolean;
	hit_breakpoint_ids?: number[];
}

export interface DebugSession {
	session_id: string;
	adapter: string;
	language: string;
	target?: string;
	mode?: string;
	request: string;
	io: "output" | "terminal";
	terminal_id?: string;
	capabilities: {
		supports_step_back: boolean;
	};
	state_version: number;
	state: "starting" | "configuring" | "running" | "stopped" | "terminated";
	stop?: DebugStop;
	exit_code?: number;
	started_at: string;
	error?: string;
}

export interface DebugPlanBreakpoint {
	file_path: string;
	line: number;
	column?: number;
	condition?: string;
	hit_condition?: string;
	log_message?: string;
}

export interface DebugLaunchPlan {
	/** Host-side reviewed plan; launches refer to it instead of trusting this copy. */
	id?: string;
	action: DebugAction;
	title: string;
	adapter: string;
	terminal_available: boolean;
	project_dir: string;
	request: "launch" | "attach";
	io: "output" | "terminal";
	configuration: Record<string, unknown>;
	breakpoints: DebugPlanBreakpoint[];
	function_breakpoints: string[];
	prelaunch?: {
		title: string;
		command: string;
		args?: string[];
		ready_url?: string;
		wait_for_exit?: boolean;
	};
}

export interface DebugToolProgress {
	tool: string;
	label: string;
	phase: "checking" | "installing" | "updating";
	current: number;
	total: number;
}

export interface DebugToolStatus {
	tool: string;
	label: string;
	installed: boolean;
	installable: boolean;
	unavailable_reason?: string;
}

export type DebugPlanResult =
	| { type: "plan"; plan: DebugLaunchPlan; warning?: string }
	| { type: "installation_required"; tools: DebugToolStatus[] };

type DebugPlanEvent =
	| DebugPlanResult
	| { type: "progress"; progress: DebugToolProgress }
	| { type: "tools"; tools: DebugToolStatus[] }
	| { type: "error"; error: string };

export interface DebugSourceBreakpoint {
	line: number;
	column?: number;
	condition?: string;
	hit_condition?: string;
	log_message?: string;
}

export interface DebugStackFrame {
	id: number;
	name: string;
	source?: { name?: string; path?: string };
	line: number;
	column: number;
}

export interface DebugState {
	available: boolean;
	session?: DebugSession;
	frame?: DebugStackFrame;
	breakpoints: DebugSourceBreakpoint[];
	resolved?: DebugBreakpoint[];
	frame_error?: string;
}

export interface DebugEvaluation {
	result: string;
	type?: string;
	variables_reference?: number;
	named_variables?: number;
	indexed_variables?: number;
}

export interface DebugBreakpoint {
	id?: number;
	verified: boolean;
	message?: string;
	line?: number;
	column?: number;
	reason?: "pending" | "failed";
}

export interface DebugThread {
	id: number;
	name: string;
}

export interface DebugScope {
	name: string;
	variables_reference: number;
	named_variables?: number;
	indexed_variables?: number;
	expensive?: boolean;
}

export interface DebugVariable {
	name: string;
	value: string;
	type?: string;
	evaluate_name?: string;
	variables_reference?: number;
	named_variables?: number;
	indexed_variables?: number;
}

export interface DebugScopeInspection {
	scope: DebugScope;
	variables: DebugVariable[];
	error?: string;
}

export interface DebugInspection {
	session?: DebugSession;
	output: string;
	threads: DebugThread[];
	frames: DebugStackFrame[];
	error?: string;
}

export async function discoverDebugTargets(
	path: string,
	content: string,
	signal?: AbortSignal,
) {
	return fetchJSON<{ targets: DebugTarget[] }>("/api/workspace/debug/targets", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ path, content }),
		signal,
	});
}

export async function generateDebugPlan(
	request: {
		action: DebugAction;
		target_id: string;
		current_path: string;
		install?: boolean;
	},
	signal?: AbortSignal,
	onProgress?: (progress: DebugToolProgress) => void,
	onTools?: (tools: DebugToolStatus[]) => void,
): Promise<DebugPlanResult> {
	const response = await fetchOK("/api/workspace/debug/plan", {
		method: "POST",
		headers: {
			"Content-Type": "application/json",
			Accept: "application/x-ndjson",
		},
		body: JSON.stringify(request),
		signal,
	});
	if (!response.body) throw new Error("Launch preparation response was empty.");

	const reader = response.body.getReader();
	const decoder = new TextDecoder();
	let buffer = "";
	let result: DebugPlanResult | undefined;
	const accept = (line: string) => {
		signal?.throwIfAborted();
		if (!line.trim()) return;
		const event = JSON.parse(line) as DebugPlanEvent;
		if (event.type === "progress") onProgress?.(event.progress);
		else if (event.type === "tools") onTools?.(event.tools ?? []);
		else if (event.type === "plan" || event.type === "installation_required")
			result = event;
		else if (event.type === "error") throw new Error(event.error);
	};
	try {
		while (true) {
			const { value, done } = await reader.read();
			signal?.throwIfAborted();
			buffer += decoder.decode(value, { stream: !done });
			let newline = buffer.indexOf("\n");
			while (newline >= 0) {
				accept(buffer.slice(0, newline));
				buffer = buffer.slice(newline + 1);
				newline = buffer.indexOf("\n");
			}
			if (done) break;
		}
		accept(buffer);
	} finally {
		await reader.cancel().catch(() => {});
		reader.releaseLock();
	}
	if (!result)
		throw new Error(
			"Launch preparation ended before a plan was ready. Please retry.",
		);
	return result;
}

export function startDebugPlan(plan: DebugLaunchPlan, signal?: AbortSignal) {
	return workspaceClient().dap.launch(plan, signal);
}
export function getDebugState(path?: string, signal?: AbortSignal) {
	return workspaceClient().dap.state(path, signal);
}
export function getDebugSession(signal?: AbortSignal) {
	return workspaceClient().dap.status(undefined, signal);
}
export function getDebugInspection(signal?: AbortSignal) {
	return workspaceClient().dap.inspection(true, signal);
}
export function getDebugOutput(signal?: AbortSignal) {
	return workspaceClient().dap.inspection(false, signal);
}
export function setDebugBreakpoints(
	path: string,
	breakpoints: DebugSourceBreakpoint[],
	signal?: AbortSignal,
) {
	return workspaceClient().dap.breakpoints(path, breakpoints, signal);
}
export function controlDebug(
	operation:
		| "continue"
		| "next"
		| "stepIn"
		| "stepOut"
		| "stepBack"
		| "pause"
		| "stop",
	sessionId?: string,
	threadId?: number,
	signal?: AbortSignal,
) {
	return workspaceClient().dap.control(operation, sessionId, threadId, signal);
}
export function evaluateDebug(
	expression: string,
	sessionId?: string,
	frameId?: number,
	stateVersion?: number,
	signal?: AbortSignal,
) {
	return workspaceClient().dap.evaluate(
		expression,
		sessionId,
		frameId,
		stateVersion,
		signal,
	);
}
export function getDebugScopes(
	frameId: number,
	sessionId?: string,
	stateVersion?: number,
	signal?: AbortSignal,
) {
	return workspaceClient().dap.scopes(frameId, sessionId, stateVersion, signal);
}
export function getDebugVariables(
	variablesReference: number,
	sessionId?: string,
	stateVersion?: number,
	signal?: AbortSignal,
) {
	return workspaceClient().dap.variables(
		variablesReference,
		sessionId,
		stateVersion,
		signal,
	);
}
