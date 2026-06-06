import { Brain } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ServerMessage } from "../types/protocol";

interface ModelInfo {
	id: string;
	name: string;
}

interface Props {
	sessionId?: string;
	subscribe?: (handler: (msg: ServerMessage) => void) => () => void;
}

export function ModelPicker({ sessionId, subscribe }: Props) {
	const [model, setModel] = useState("");
	const [models, setModels] = useState<ModelInfo[]>([]);
	const [effort, setEffort] = useState("auto");
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

	const apiBase = sessionId
		? `/api/sessions/${encodeURIComponent(sessionId)}`
		: "/api";

	const loadCurrent = useCallback(() => {
		fetch(`${apiBase}/model`)
			.then((r) => r.json())
			.then((data) => setModel(data.model || ""))
			.catch(() => {});
		fetch(`${apiBase}/effort`)
			.then((r) => r.json())
			.then((data) => {
				applyEffort(data.effort);
				setEffortOptions(Array.isArray(data.options) ? data.options : []);
			})
			.catch(() => {});
	}, [applyEffort, apiBase]);

	useEffect(() => {
		loadCurrent();
		loadModels();
	}, [loadCurrent, loadModels]);

	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "agent_changed" || msg.type === "model_changed") {
				loadCurrent();
				loadModels();
			}
		});
	}, [subscribe, loadCurrent, loadModels]);

	const toggle = useCallback(() => {
		setOpen((v) => !v);
	}, []);

	const selectModel = useCallback(
		(id: string) => {
			fetch(`${apiBase}/model`, {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ model: id }),
			})
				.then((r) => r.json())
				.then((data) => setModel(data.model || id))
				.catch(() => {});
		},
		[apiBase],
	);

	const selectEffort = useCallback(
		(value: string) => {
			fetch(`${apiBase}/effort`, {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ effort: value }),
			})
				.then((r) => r.json())
				.then((data) => applyEffort(data.effort))
				.catch(() => {});
		},
		[applyEffort, apiBase],
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

	const defaultEffort = useMemo(
		() =>
			effortOptions.find((v) => v === "default" || v === "auto") ?? "default",
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
										onClick={() =>
											selectEffort(v === effort ? defaultEffort : v)
										}
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
