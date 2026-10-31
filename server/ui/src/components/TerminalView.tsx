import { FitAddon } from "@xterm/addon-fit";
import { Terminal, type ITheme } from "@xterm/xterm";
import { useEffect, useEffectEvent, useRef, useState } from "react";
import {
	hostTerminalClientId,
	terminalURI,
	ownTerminalClaim,
} from "../api/terminals.ts";
import { workspaceClient } from "../state/workspaceClient.ts";
import { ActionType } from "../state/ahpValues.ts";
import type { TerminalState } from "@microsoft/agent-host-protocol";
import { useColorScheme } from "../hooks/useColorScheme";

interface Props {
	id: string;
	active: boolean;
	onExit: (id: string) => void;
	onTitle: (id: string, title: string) => void;
}

export function TerminalView({ id, active, onExit, onTitle }: Props) {
	const hostRef = useRef<HTMLDivElement>(null);
	const termRef = useRef<Terminal | null>(null);
	const fitRef = useRef<FitAddon | null>(null);
	const scheme = useColorScheme();
	const [status, setStatus] = useState("");

	const handleTitle = useEffectEvent(onTitle);
	const handleExit = useEffectEvent(onExit);

	useEffect(() => {
		const host = hostRef.current;
		if (!host) return;

		const term = new Terminal({
			fontFamily: cssVar("--font-mono", "monospace"),
			fontSize: 12,
			lineHeight: 1.25,
			cursorBlink: true,
			scrollback: 10000,
			theme: terminalTheme(
				document.documentElement.dataset.theme === "light" ? "light" : "dark",
			),
		});
		const fit = new FitAddon();
		term.loadAddon(fit);
		term.open(host);
		safeFit(fit);

		termRef.current = term;
		fitRef.current = fit;

		const ahp = workspaceClient().ahp,
			uri = terminalURI(id);
		let owned = false,
			exited = false,
			sawRunning = false,
			replaying = false,
			stopped = false;
		term.options.disableStdin = true;
		const report = (error: unknown) => {
			if (!stopped)
				setStatus(error instanceof Error ? error.message : String(error));
		};
		const resize = () => {
			if (owned && !exited && ahp.getConnected())
				void ahp
					.dispatch(uri, {
						type: ActionType.TerminalResized,
						cols: term.cols,
						rows: term.rows,
					})
					.catch(report);
		};
		let claiming = false;
		const release = ahp.watch(uri, (event) => {
			const state = event.state as TerminalState;
			const wasOwned = owned,
				wasExited = exited;
			owned =
				state.claim.kind === "client" && state.claim.clientId === ahp.clientId;
			exited = state.lifecycle.status === "exited";
			term.options.disableStdin = !owned || exited || !ahp.getConnected();
			if (event.snapshot) {
				term.reset();
				// Queries in replayed output must not answer into the live shell.
				replaying = true;
				term.write(
					state.content
						.map((part) => (part.type === "command" ? part.output : part.value))
						.join(""),
					() => {
						replaying = false;
					},
				);
				resize();
			} else if (event.action?.action.type === "terminal/data")
				term.write(event.action.action.data);
			else if (event.action?.action.type === "terminal/cleared") term.reset();
			// A host-created or handed-over terminal takes this client's size.
			if (owned && !wasOwned && !event.snapshot) resize();
			if (!exited) sawRunning = true;
			else if (sawRunning && !wasExited) handleExit(id);
			if (
				event.snapshot ||
				event.action?.action.type === "terminal/titleChanged"
			)
				handleTitle(id, state.title);
			setStatus(
				exited
					? `Exited${state.lifecycle.status === "exited" && state.lifecycle.exitCode !== undefined ? ` (${state.lifecycle.exitCode})` : ""}`
					: owned
						? ""
						: "Read only · owned by another client",
			);
			const handedOver =
				event.action?.action.type === "terminal/claimed" &&
				state.claim.kind === "client" &&
				state.claim.clientId === hostTerminalClientId;
			if (!owned && !claiming && (event.snapshot || handedOver)) {
				claiming = true;
				void ahp
					.dispatch(uri, {
						type: ActionType.TerminalClaimed,
						claim: ownTerminalClaim(),
					})
					.catch(() => {})
					.finally(() => {
						claiming = false;
					});
			}
		});
		void ahp.read<TerminalState>(uri).catch(report);
		const releaseConnection = ahp.subscribeConnection(() => {
			term.options.disableStdin = true;
			if (!ahp.getConnected())
				setStatus(
					ahp.getReplaced()
						? "Workspace changed. Reload to reconnect."
						: "Reconnecting terminal…",
				);
		});
		const dataSub = term.onData((data) => {
			if (owned && !exited && !replaying && ahp.getConnected())
				void ahp
					.dispatch(uri, { type: ActionType.TerminalInput, data })
					.catch(report);
		});
		const resizeSub = term.onResize(resize);

		// OSC 0/2 — shells and full-screen apps name their window this way. The
		// scrollback replay re-emits the last one, so a reattach keeps the name.
		const titleSub = term.onTitleChange((title) =>
			handleTitle(id, title.trim().slice(0, 80)),
		);

		const observer = new ResizeObserver(() => {
			if (host.clientWidth === 0 || host.clientHeight === 0) return;
			safeFit(fit);
		});
		observer.observe(host);

		return () => {
			stopped = true;
			release();
			releaseConnection();
			observer.disconnect();
			dataSub.dispose();
			resizeSub.dispose();
			titleSub.dispose();

			term.dispose();
			termRef.current = null;
			fitRef.current = null;
		};
	}, [id]);

	useEffect(() => {
		const term = termRef.current;
		if (term) term.options.theme = terminalTheme(scheme);
	}, [scheme]);

	useEffect(() => {
		if (!active) return;
		const handle = window.setTimeout(() => {
			if (fitRef.current) safeFit(fitRef.current);
			termRef.current?.focus();
		}, 0);
		return () => window.clearTimeout(handle);
	}, [active]);

	return (
		<div className="relative h-full w-full bg-bg">
			<div ref={hostRef} className="h-full w-full px-2 py-1" />
			{status && (
				<div
					role="status"
					className="absolute top-2 right-2 rounded bg-bg-elevated px-2 py-1 text-[11px] text-fg-muted"
				>
					{status}
				</div>
			)}
		</div>
	);
}

function safeFit(fit: FitAddon) {
	try {
		fit.fit();
	} catch {}
}

function cssVar(name: string, fallback: string): string {
	if (typeof window === "undefined") return fallback;
	const value = getComputedStyle(document.documentElement)
		.getPropertyValue(name)
		.trim();
	return value || fallback;
}

const DARK_ANSI: ITheme = {
	black: "#1c1c1c",
	red: "#f87171",
	green: "#34d399",
	yellow: "#fbbf24",
	blue: "#60a5fa",
	magenta: "#a78bfa",
	cyan: "#22d3ee",
	white: "#d4d4d4",
	brightBlack: "#525252",
	brightRed: "#fca5a5",
	brightGreen: "#6ee7b7",
	brightYellow: "#fcd34d",
	brightBlue: "#93c5fd",
	brightMagenta: "#c4b5fd",
	brightCyan: "#67e8f9",
	brightWhite: "#fafafa",
};

const LIGHT_ANSI: ITheme = {
	black: "#171717",
	red: "#dc2626",
	green: "#059669",
	yellow: "#b45309",
	blue: "#2563eb",
	magenta: "#7c3aed",
	cyan: "#0891b2",
	white: "#525252",
	brightBlack: "#737373",
	brightRed: "#ef4444",
	brightGreen: "#10b981",
	brightYellow: "#d97706",
	brightBlue: "#3b82f6",
	brightMagenta: "#8b5cf6",
	brightCyan: "#06b6d4",
	brightWhite: "#171717",
};

function terminalTheme(scheme: "light" | "dark"): ITheme {
	const light = scheme === "light";
	const background = cssVar("--color-bg", light ? "#ffffff" : "#0a0a0a");
	const foreground = cssVar("--color-fg", light ? "#171717" : "#e6e6e6");
	return {
		...(light ? LIGHT_ANSI : DARK_ANSI),
		background,
		foreground,
		cursor: foreground,
		cursorAccent: background,
		selectionBackground: cssVar(
			"--color-selection",
			light ? "rgba(0,0,0,0.15)" : "rgba(255,255,255,0.15)",
		),
	};
}
