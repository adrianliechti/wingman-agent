export interface PendingImage {
	id: string;
	dataUrl: string;
	name?: string;
}

function readImageAsDataUrl(file: File): Promise<string> {
	return new Promise((resolve, reject) => {
		const reader = new FileReader();
		reader.onload = () => resolve(reader.result as string);
		reader.onerror = () => reject(reader.error);
		reader.readAsDataURL(file);
	});
}

const MAX_EDGE = 2048;
const JPEG_QUALITY = 0.9;
const WEBP_QUALITY = 0.9;
const PASSTHROUGH_MAX_BYTES = 512 * 1024;
const PNG_PASSTHROUGH_MAX_BYTES = 4 * 1024 * 1024;

export async function processImage(file: File): Promise<string> {
	if (file.type === "image/gif") return readImageAsDataUrl(file);

	let bitmap: ImageBitmap;
	try {
		bitmap = await createImageBitmap(file);
	} catch {
		return readImageAsDataUrl(file);
	}

	try {
		const longest = Math.max(bitmap.width, bitmap.height);
		const isAlphaCapable =
			file.type === "image/png" || file.type === "image/webp";
		const maxPassthrough = isAlphaCapable
			? PNG_PASSTHROUGH_MAX_BYTES
			: PASSTHROUGH_MAX_BYTES;
		if (longest <= MAX_EDGE && file.size <= maxPassthrough) {
			return readImageAsDataUrl(file);
		}
		const scale = Math.min(1, MAX_EDGE / longest);
		const w = Math.max(1, Math.round(bitmap.width * scale));
		const h = Math.max(1, Math.round(bitmap.height * scale));
		const canvas = document.createElement("canvas");
		canvas.width = w;
		canvas.height = h;
		const ctx = canvas.getContext("2d");
		if (!ctx) return readImageAsDataUrl(file);
		ctx.drawImage(bitmap, 0, 0, w, h);
		if (file.type === "image/webp") {
			return canvas.toDataURL("image/webp", WEBP_QUALITY);
		}
		if (file.type === "image/png") {
			return canvas.toDataURL("image/png");
		}
		return canvas.toDataURL("image/jpeg", JPEG_QUALITY);
	} finally {
		bitmap.close();
	}
}
