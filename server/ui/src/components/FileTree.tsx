import {
	ChevronDown,
	ChevronRight,
	ClipboardCopy,
	Copy,
	Download,
	File,
	FileText,
	Folder,
	FolderOpen,
	Pencil,
	Trash2,
} from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import type { FileEntry, ServerMessage } from "../types/protocol";
import { getDeviconClass } from "../utils/fileIcons";

interface Props {
	onFileSelect: (path: string) => void;
	subscribe?: (handler: (msg: ServerMessage) => void) => () => void;
}

interface TreeNode extends FileEntry {
	children?: TreeNode[];
	expanded?: boolean;
	loaded?: boolean;
}

interface MenuState {
	x: number;
	y: number;
	node: TreeNode;
}

export function FileTree({ onFileSelect, subscribe }: Props) {
	const [nodes, setNodes] = useState<TreeNode[]>([]);
	const [menu, setMenu] = useState<MenuState | null>(null);
	const [renaming, setRenaming] = useState<string | null>(null);
	const [renameValue, setRenameValue] = useState("");
	const nodesRef = useRef(nodes);
	useEffect(() => {
		nodesRef.current = nodes;
	});

	const loadDir = useCallback(async (dirPath: string): Promise<TreeNode[]> => {
		const res = await fetch(
			`/api/files?path=${encodeURIComponent(dirPath || "")}`,
		);
		const files: FileEntry[] = await res.json();

		return files
			.sort((a, b) => {
				if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
				return a.name.localeCompare(b.name);
			})
			.map((f) => ({ ...f, expanded: false, loaded: false }));
	}, []);

	useEffect(() => {
		loadDir("").then(setNodes);
	}, [loadDir]);

	// Refresh the tree while preserving expansion state. Walks every previously
	// loaded directory, refetches its contents, and recursively rebuilds the
	// children of expanded folders. New entries appear, removed ones disappear.
	const refresh = useCallback(async () => {
		const refreshLevel = async (
			path: string,
			prev: TreeNode[],
		): Promise<TreeNode[]> => {
			const fresh = await loadDir(path);
			const prevByPath = new Map(prev.map((n) => [n.path, n]));
			const result: TreeNode[] = [];
			for (const f of fresh) {
				const old = prevByPath.get(f.path);
				if (old?.is_dir && old.expanded && old.loaded && old.children) {
					const newChildren = await refreshLevel(f.path, old.children);
					result.push({
						...f,
						expanded: true,
						loaded: true,
						children: newChildren,
					});
				} else {
					result.push({ ...f, expanded: false, loaded: false });
				}
			}
			return result;
		};
		setNodes(await refreshLevel("", nodesRef.current));
	}, [loadDir]);

	useEffect(() => {
		if (!subscribe) return;
		return subscribe((msg) => {
			if (msg.type === "files_changed") {
				refresh();
			}
		});
	}, [subscribe, refresh]);

	const toggleDir = useCallback(
		async (path: string) => {
			const toggle = async (items: TreeNode[]): Promise<TreeNode[]> => {
				const result: TreeNode[] = [];
				for (const node of items) {
					if (node.path === path && node.is_dir) {
						if (!node.loaded) {
							const children = await loadDir(node.path);
							result.push({ ...node, expanded: true, loaded: true, children });
						} else {
							result.push({ ...node, expanded: !node.expanded });
						}
					} else if (node.children) {
						result.push({ ...node, children: await toggle(node.children) });
					} else {
						result.push(node);
					}
				}
				return result;
			};
			setNodes(await toggle(nodes));
		},
		[nodes, loadDir],
	);

	// Close the context menu on any click/scroll/Escape outside it. The menu
	// sets stopPropagation on its own clicks so they don't bubble here.
	useEffect(() => {
		if (!menu) return;
		const close = () => setMenu(null);
		const onKey = (e: KeyboardEvent) => {
			if (e.key === "Escape") close();
		};
		document.addEventListener("mousedown", close);
		document.addEventListener("scroll", close, true);
		document.addEventListener("keydown", onKey);
		return () => {
			document.removeEventListener("mousedown", close);
			document.removeEventListener("scroll", close, true);
			document.removeEventListener("keydown", onKey);
		};
	}, [menu]);

	const beginRename = (node: TreeNode) => {
		setRenaming(node.path);
		setRenameValue(node.name);
		setMenu(null);
	};

	const commitRename = async (node: TreeNode) => {
		const newName = renameValue.trim();
		setRenaming(null);
		if (!newName || newName === node.name) return;
		if (newName.includes("/") || newName.includes("\\")) return;

		const parent = node.path.includes("/")
			? node.path.slice(0, node.path.lastIndexOf("/"))
			: "";
		const to = parent ? `${parent}/${newName}` : newName;

		const res = await fetch("/api/files/rename", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ from: node.path, to }),
		});
		if (!res.ok) {
			alert(`Rename failed: ${await res.text()}`);
		}
	};

	const handleDelete = async (node: TreeNode) => {
		setMenu(null);
		const label = node.is_dir ? "folder" : "file";
		if (!confirm(`Delete ${label} "${node.name}"?`)) return;
		const res = await fetch(
			`/api/files?path=${encodeURIComponent(node.path)}`,
			{ method: "DELETE" },
		);
		if (!res.ok) {
			alert(`Delete failed: ${await res.text()}`);
		}
	};

	const handleDuplicate = async (node: TreeNode) => {
		setMenu(null);
		const dot = node.is_dir ? -1 : node.name.lastIndexOf(".");
		const stem = dot > 0 ? node.name.slice(0, dot) : node.name;
		const ext = dot > 0 ? node.name.slice(dot) : "";
		const parent = node.path.includes("/")
			? node.path.slice(0, node.path.lastIndexOf("/"))
			: "";

		// Probe candidate names client-side until the server accepts one. The
		// backend rejects collisions with 409; cap retries so a pathological
		// directory can't lock the UI in a loop.
		for (let i = 1; i <= 50; i++) {
			const suffix = i === 1 ? " copy" : ` copy ${i}`;
			const candidate = `${stem}${suffix}${ext}`;
			const to = parent ? `${parent}/${candidate}` : candidate;
			const res = await fetch("/api/files/copy", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ from: node.path, to }),
			});
			if (res.ok) return;
			if (res.status !== 409) {
				alert(`Copy failed: ${await res.text()}`);
				return;
			}
		}
		alert("Copy failed: too many existing copies");
	};

	const handleCopy = async (node: TreeNode) => {
		setMenu(null);
		// The fetch resolves inside the ClipboardItem's promise rather than
		// before navigator.clipboard.write — Safari/Firefox revoke the user
		// gesture while we await, throwing "not allowed by user agent". The
		// promise-valued ClipboardItem form lets the browser track the gesture
		// through the async work.
		try {
			await navigator.clipboard.write([
				new ClipboardItem({
					"text/plain": fetch(
						`/api/files/read?path=${encodeURIComponent(node.path)}`,
					).then(async (res) => {
						if (!res.ok) throw new Error(await res.text());
						const data = (await res.json()) as { content: string };
						return new Blob([data.content], { type: "text/plain" });
					}),
				}),
			]);
		} catch (e) {
			alert(`Copy failed: ${e instanceof Error ? e.message : String(e)}`);
		}
	};

	const handleDownload = (node: TreeNode) => {
		setMenu(null);
		const a = document.createElement("a");
		a.href = `/api/files/download?path=${encodeURIComponent(node.path)}`;
		a.download = node.name;
		document.body.appendChild(a);
		a.click();
		document.body.removeChild(a);
	};

	const renderNodes = (items: TreeNode[], depth: number) => {
		return items.map((node) => {
			const isRenaming = renaming === node.path;
			return (
				<div key={node.path}>
					<div
						className="flex items-center gap-1 py-[3px] pr-2 cursor-pointer text-fg-muted whitespace-nowrap text-[12px] leading-snug select-none hover:bg-bg-hover hover:text-fg transition-colors"
						style={{ paddingLeft: 12 + depth * 12 }}
						onClick={() => {
							if (isRenaming) return;
							node.is_dir ? toggleDir(node.path) : onFileSelect(node.path);
						}}
						onContextMenu={(e) => {
							e.preventDefault();
							e.stopPropagation();
							setMenu({ x: e.clientX, y: e.clientY, node });
						}}
						title={node.name}
					>
						<span className="w-3.5 flex items-center justify-center shrink-0 text-fg-dim">
							{node.is_dir ? (
								node.expanded ? (
									<ChevronDown size={12} />
								) : (
									<ChevronRight size={12} />
								)
							) : null}
						</span>
						<span
							className={`shrink-0 flex items-center justify-center w-3.5 h-3.5 ${node.is_dir ? "text-fg-muted" : "text-fg-dim"}`}
						>
							{node.is_dir ? (
								node.expanded ? (
									<FolderOpen size={14} />
								) : (
									<Folder size={14} />
								)
							) : (() => {
								const cls = getDeviconClass(node.name);
								if (cls) {
									return <i className={`${cls} text-[14px] leading-none`} />;
								}
								return <File size={13} />;
							})()}
						</span>
						{isRenaming ? (
							<input
								autoFocus
								value={renameValue}
								onChange={(e) => setRenameValue(e.target.value)}
								onClick={(e) => e.stopPropagation()}
								onKeyDown={(e) => {
									if (e.key === "Enter") commitRename(node);
									else if (e.key === "Escape") setRenaming(null);
								}}
								onBlur={() => commitRename(node)}
								className="ml-0.5 bg-bg-surface border border-border-strong rounded px-1 py-0 text-fg text-[12px] outline-none min-w-0 flex-1"
							/>
						) : (
							<span
								className={`overflow-hidden text-ellipsis ml-0.5 ${node.is_dir ? "text-fg" : ""}`}
							>
								{node.name}
							</span>
						)}
					</div>
					{node.expanded &&
						node.children &&
						renderNodes(node.children, depth + 1)}
				</div>
			);
		});
	};

	return (
		<div className="flex-1 overflow-y-auto py-2 bg-bg relative">
			{renderNodes(nodes, 0)}
			{menu && (
				<ContextMenu
					menu={menu}
					onOpen={() => {
						setMenu(null);
						onFileSelect(menu.node.path);
					}}
					onRename={() => beginRename(menu.node)}
					onDuplicate={() => handleDuplicate(menu.node)}
					onCopy={() => handleCopy(menu.node)}
					onDownload={() => handleDownload(menu.node)}
					onDelete={() => handleDelete(menu.node)}
				/>
			)}
		</div>
	);
}

interface ContextMenuProps {
	menu: MenuState;
	onOpen: () => void;
	onRename: () => void;
	onDuplicate: () => void;
	onCopy: () => void;
	onDownload: () => void;
	onDelete: () => void;
}

function ContextMenu({
	menu,
	onOpen,
	onRename,
	onDuplicate,
	onCopy,
	onDownload,
	onDelete,
}: ContextMenuProps) {
	const isDir = menu.node.is_dir;
	return (
		<div
			className="fixed z-100 min-w-[160px] bg-bg-elevated border border-border-subtle rounded-md shadow-2xl py-1 text-[12px]"
			style={{ left: menu.x, top: menu.y }}
			onMouseDown={(e) => e.stopPropagation()}
			onContextMenu={(e) => e.preventDefault()}
		>
			{!isDir && (
				<MenuItem icon={<FileText size={12} />} label="Open" onClick={onOpen} />
			)}
			{!isDir && (
				<MenuItem
					icon={<ClipboardCopy size={12} />}
					label="Copy"
					onClick={onCopy}
				/>
			)}
			<MenuItem icon={<Pencil size={12} />} label="Rename" onClick={onRename} />
			<MenuItem
				icon={<Copy size={12} />}
				label="Duplicate"
				onClick={onDuplicate}
			/>
			{!isDir && (
				<MenuItem
					icon={<Download size={12} />}
					label="Download"
					onClick={onDownload}
				/>
			)}
			<div className="my-1 border-t border-border-subtle" />
			<MenuItem
				icon={<Trash2 size={12} />}
				label="Delete"
				onClick={onDelete}
				danger
			/>
		</div>
	);
}

function MenuItem({
	icon,
	label,
	onClick,
	danger,
}: {
	icon: React.ReactNode;
	label: string;
	onClick: () => void;
	danger?: boolean;
}) {
	return (
		<button
			type="button"
			className={`w-full flex items-center gap-2 px-3 py-1.5 text-left hover:bg-bg-hover transition-colors ${danger ? "text-red-400 hover:text-red-300" : "text-fg-muted hover:text-fg"}`}
			onClick={onClick}
		>
			<span className="w-3.5 flex items-center justify-center shrink-0">
				{icon}
			</span>
			<span>{label}</span>
		</button>
	);
}
