// Git-style status letters (A, D, M, ?, …) share one color across change views.
export function statusColor(status: string, conflict?: boolean) {
	if (conflict) return "text-danger";
	if (status === "A" || status === "?") return "text-success";
	if (status === "D") return "text-danger";
	return "text-warning";
}
