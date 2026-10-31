export interface WorkspaceFileTarget {
	path: string;
	external: boolean;
}

export function isAbsolutePath(path: string): boolean {
	return /^(\/|[a-z]:[\\/])/i.test(path);
}

// Paths inside the workspace become relative; anything else stays absolute and external.
export function workspaceFileTarget(
	path: string,
	root: string,
): WorkspaceFileTarget {
	const target = path.replaceAll("\\", "/");
	const normalizedRoot = root.replaceAll("\\", "/").replace(/\/+$/, "");
	const caseInsensitive = /^[a-z]:\//i.test(normalizedRoot);
	const comparableTarget = caseInsensitive ? target.toLowerCase() : target;
	const comparableRoot = caseInsensitive
		? normalizedRoot.toLowerCase()
		: normalizedRoot;
	if (
		comparableTarget === comparableRoot ||
		comparableTarget.startsWith(`${comparableRoot}/`)
	) {
		const relative = resolveDots(target.slice(normalizedRoot.length));
		if (relative !== null) return { path: relative, external: false };
	}
	return { path: target, external: true };
}

// Resolves "." and ".." segments; null when the path climbs above its start.
function resolveDots(path: string): string | null {
	const segments: string[] = [];
	for (const segment of path.split("/")) {
		if (segment === "" || segment === ".") continue;
		if (segment !== "..") segments.push(segment);
		else if (segments.pop() === undefined) return null;
	}
	return segments.join("/");
}
