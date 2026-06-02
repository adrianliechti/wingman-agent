import { Brain } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ServerMessage } from "../types/protocol";

interface ModelInfo {
	id: string;
	name: string;
}

interface Props {
	// subscribe lets the picker refetch its catalog when the active
	// agent changes (the new backend's model/effort options differ).
	// Optional so non-WebSocket callers can still render the component.
	subscribe?: (handler: (msg: ServerMessage) => void) => () => void;
}

export function ModelPicker({ subscribe }: Props) {
	const [model, setModel] = useState("");
	const [models, setModels] = useState<ModelInfo[]>([]);
	const [effort, setEffort] = useState("auto");
	// Empty until /api/effort reports the active backend's set; the effort row
	// stays hidden until then, so we never flash a guessed list.
	const [effortOptions, setEffortOptions] = useState<string[]>([]);
	const [open, setOpen] = useState(false);
	const popRef = useRef<HTMLDivElement>(null);
	const btnRef = useRef<HTMLButtonElement>(null);

	const applyEffort = useCallback((v: unknown) => {
		if (typeof v === "string" && v !== "") {
			setEffort(v);
		} else {
			setEffort("auto");
		}
	}, []);

	const loadModels = useCallback(() => {
		fetch("/api/models")
			.then((r) => r.json())
			.then((data: ModelInfo[]) => setModels(data))
			.catch(() => setModels([]));
	}, []);

	const loadCurrent = useCallback(() => {
		fetch("/api/model")
			.then((r) => r.json())
			.then((data) => setModel(data.model || ""))
			.catch(() => {});
		fetch("/api/effort")
			.then((r) => r.json())
			.then((data) => {
				applyEffort(data.effort);
				// The backend advertises its own effort set (empty = no effort
				// selector). Drive the buttons from it rather than a hard-coded list.
				setEffortOptions(Array.isArray(data.options) ? data.options : []);
			})
			.catch(() => {});
	}, [applyEffort]);

	useEffect(() => {
		loadCurrent();
		loadModels();
	}, [loadCurrent, loadModels]);

	// Refetch the catalog when the backend signals it changed:
	//   • agent_changed — a different backend with its own model/effort set.
	//   • model_changed — the upstream catalog finished loading (startup) or
	//     narrowed, which can move the default selection.
	// Both publish through the same /api/* endpoints (the server dispatches).
	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "agent_changed" || msg.type === "model_changed") {
				// loadCurrent refetches /api/effort, which now carries the active
				// backend's effort options too — ACP backends may use a different
				// set than wingman's auto/low/medium/high, or none at all.
				loadCurrent();
				loadModels();
			}
		});
	}, [subscribe, loadCurrent, loadModels]);

	const toggle = useCallback(() => {
		setOpen((v) => !v);
	}, []);

	const selectModel = useCallback((id: string) => {
		fetch("/api/model", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ model: id }),
		})
			.then((r) => r.json())
			.then((data) => setModel(data.model || id))
			.catch(() => {});
	}, []);

	const selectEffort = useCallback(
		(value: string) => {
			fetch("/api/effort", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ effort: value }),
			})
				.then((r) => r.json())
				.then((data) => applyEffort(data.effort))
				.catch(() => {});
		},
		[applyEffort],
	);

	useEffect(() => {
		if (!open) return;
		const handler = (e: MouseEvent) => {
			const target = e.target as Node;
			if (
				popRef.current &&
				!popRef.current.contains(target) &&
				btnRef.current &&
				!btnRef.current.contains(target)
			) {
				setOpen(false);
			}
		};
		document.addEventListener("mousedown", handler);
		return () => document.removeEventListener("mousedown", handler);
	}, [open]);

	const currentName = useMemo(() => {
		const match = models.find((m) => m.id === model);
		return match?.name || model;
	}, [models, model]);

	// The backend's "default"/"auto" sentinel is the deselected state — hide it
	// as an explicit button. Deselecting the active level falls back to it.
	const defaultEffort = useMemo(
		() => effortOptions.find((v) => v === "default" || v === "auto") ?? "default",
		[effortOptions],
	);
	const efforts = useMemo(
		() => effortOptions.filter((v) => v !== defaultEffort),
		[effortOptions, defaultEffort],
	);

	if (!model) return null;

	return (
		<div className="relative">
			<button
				ref={btnRef}
				type="button"
				onClick={toggle}
				className="flex items-center gap-1 px-2 h-7 rounded text-[11.5px] text-fg-muted hover:text-fg hover:bg-bg-hover cursor-pointer transition-colors max-w-[260px]"
				title={`${model} · ${effort}`}
			>
				<Brain size={12} className="shrink-0" />
				<span className="truncate">{currentName}</span>
				{effort !== "auto" && effort !== "default" && (
					<>
						<span className="text-fg-dim">·</span>
						<span className="capitalize text-fg-dim">{effort}</span>
					</>
				)}
			</button>
			{open && (
				<div
					ref={popRef}
					className="absolute bottom-full mb-1 left-0 min-w-[240px] max-w-[360px] bg-bg-elevated/95 backdrop-blur-sm border border-border rounded-md shadow-xl z-50"
				>
					<div className="py-1 max-h-[260px] overflow-y-auto">
						{models.length === 0 ? (
							<div className="px-3 py-2 text-[12px] text-fg-dim">Loading…</div>
						) : (
							models.map((m) => (
								<button
									type="button"
									key={m.id}
									className={`block w-full text-left px-3 py-1.5 text-[12px] cursor-pointer whitespace-nowrap transition-colors ${
										m.id === model
											? "text-fg bg-bg-active"
											: "text-fg-muted hover:text-fg hover:bg-bg-hover"
									}`}
									onClick={() => selectModel(m.id)}
								>
									{m.name}
								</button>
							))
						)}
					</div>
					{efforts.length > 0 && (
					<div className="border-t border-border px-2 py-1.5">
						<div className="flex rounded bg-bg overflow-hidden">
							{efforts.map((v) => (
								<button
									type="button"
									key={v}
									className={`flex-1 px-2 py-1 text-[11px] capitalize cursor-pointer transition-colors ${
										v === effort
											? "text-fg bg-bg-active"
											: "text-fg-muted hover:text-fg hover:bg-bg-hover"
									}`}
									onClick={() => selectEffort(v === effort ? defaultEffort : v)}
								>
									{v}
								</button>
							))}
						</div>
					</div>
					)}
				</div>
			)}
		</div>
	);
}
