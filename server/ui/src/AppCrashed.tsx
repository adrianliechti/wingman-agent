import type { ErrorInfo } from "react";
import { FolderX } from "lucide-react";
import { ErrorPanel } from "./components/ErrorScreen";

/** A window that names a project its host cannot open, such as a link to a
 * folder that is gone. It offers the way back that a desktop window lacks. */
export function ProjectUnavailable({
	project,
	error,
}: {
	project: string;
	error: Error;
}) {
	let name = project;
	try {
		// The address names a file URI or a path, with either kind of slash.
		name = decodeURIComponent(
			project
				.replace(/[\\/]+$/, "")
				.split(/[\\/]/)
				.pop()!,
		);
	} catch {
		// A malformed address is shown as it is.
	}
	return (
		<div className="flex h-full min-h-0 w-full items-center justify-center bg-bg p-5">
			<section
				role="alert"
				aria-label="Project unavailable"
				className="flex max-w-sm flex-col items-center gap-2.5 text-center"
			>
				<FolderX
					size={48}
					strokeWidth={1.45}
					aria-hidden="true"
					className="text-fg-dim"
				/>
				<h1 className="text-sm font-medium text-fg">
					{name || "This project"} could not be opened
				</h1>
				<p className="text-[12px] break-words text-fg-muted">{error.message}</p>
				<button
					type="button"
					className="mt-1 min-h-8 cursor-pointer rounded-md border border-border px-3 text-[12px] text-fg hover:bg-bg-hover"
					onClick={() => location.assign("/")}
				>
					Back to start
				</button>
			</section>
		</div>
	);
}

export function AppCrashed({
	error,
	errorInfo,
}: {
	error: Error;
	errorInfo: ErrorInfo | null;
}) {
	return (
		<ErrorPanel
			title="Wingman hit an unexpected error"
			error={error}
			errorInfo={errorInfo}
		/>
	);
}
