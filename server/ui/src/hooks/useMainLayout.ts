import { useReducer } from "react";
import { layoutReducer, type LayoutState } from "../mainLayout.ts";

// Navigation updates share one reducer so tabs and pane selections change together.
export function useMainLayout(initial: LayoutState) {
	const [layout, dispatchLayout] = useReducer(layoutReducer, initial);
	const setTabs = (
		value:
			| LayoutState["tabs"]
			| ((previous: LayoutState["tabs"]) => LayoutState["tabs"]),
	) => dispatchLayout({ field: "tabs", value });
	const setActiveTabId = (value: string | ((previous: string) => string)) =>
		dispatchLayout({ field: "activeTabId", value });
	const setLeftActiveId = (value: string | ((previous: string) => string)) =>
		dispatchLayout({ field: "leftActiveId", value });
	const setRightActiveId = (value: string | ((previous: string) => string)) =>
		dispatchLayout({ field: "rightActiveId", value });
	const setCurrentSessionId = (
		value: string | ((previous: string) => string),
	) => dispatchLayout({ field: "currentSessionId", value });
	return {
		...layout,
		setTabs,
		setActiveTabId,
		setLeftActiveId,
		setRightActiveId,
		setCurrentSessionId,
	};
}
