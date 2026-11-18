/** The browser's storage for the site, or the one that ends with the window. */
export type StorageArea = "local" | "session";

// A browser that keeps no storage throws when a page so much as names it.
const storage = (area: StorageArea) =>
	area === "local" ? localStorage : sessionStorage;

/** A stored value, or null where there is none or the browser keeps no storage. */
export function readStored(area: StorageArea, key: string): string | null {
	try {
		return storage(area).getItem(key);
	} catch {
		return null;
	}
}

/** Stores a value, or removes it when null, and reports whether the browser took it. */
export function writeStored(
	area: StorageArea,
	key: string,
	value: string | null,
): boolean {
	try {
		if (value === null) storage(area).removeItem(key);
		else storage(area).setItem(key, value);
		return true;
	} catch {
		return false;
	}
}
