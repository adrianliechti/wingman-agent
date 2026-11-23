import { X } from "lucide-react";
import { useEffect, useEffectEvent, useRef } from "react";
import { createPortal } from "react-dom";

// ImageViewer shows an image full size inside the app. Chat images are data
// URLs, which neither browsers nor the desktop shell will open in a new window.
export function ImageViewer({
	src,
	alt,
	onClose,
}: {
	src: string;
	alt: string;
	onClose: () => void;
}) {
	const closeRef = useRef<HTMLButtonElement>(null);
	const handleClose = useEffectEvent(onClose);

	useEffect(() => {
		const returnFocus = document.activeElement as HTMLElement | null;
		closeRef.current?.focus();
		// Capture Escape before it reaches the composer and stops the turn.
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key !== "Escape" || event.isComposing) return;
			event.preventDefault();
			event.stopPropagation();
			handleClose();
		};
		window.addEventListener("keydown", onKeyDown, true);
		return () => {
			window.removeEventListener("keydown", onKeyDown, true);
			returnFocus?.focus();
		};
	}, []);

	return createPortal(
		<div
			role="dialog"
			aria-modal="true"
			aria-label={alt}
			data-image-viewer
			className="fixed inset-0 z-150 flex items-center justify-center bg-bg/85 p-10"
			onMouseDown={(event) => {
				if (event.target === event.currentTarget) onClose();
			}}
		>
			<img
				src={src}
				alt={alt}
				className="block max-h-full max-w-full rounded object-contain shadow-2xl"
			/>
			<button
				ref={closeRef}
				type="button"
				onClick={onClose}
				aria-label="Close image"
				className="absolute top-3 right-3 flex h-7 w-7 items-center justify-center rounded-md text-fg-muted transition-colors hover:bg-bg-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
			>
				<X size={14} />
			</button>
		</div>,
		document.body,
	);
}
