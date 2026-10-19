import { useReducer } from "react";
import {
	layoutReducer,
	type CenterTab,
	type LayoutState,
} from "../mainLayout.ts";

// Navigation updates share one reducer so tabs and pane selections change together.
export function useMainLayout(initial: LayoutState) {
	const [layout, dispatchLayout] = useReducer(layoutReducer, initial);
	const setTabs = (
		value:
			| LayoutState["tabs"]
			| ((previous: LayoutState["tabs"]) => LayoutState["tabs"]),
	) => dispatchLayout({ type: "tabs", value });
	const setActiveTabId = (id: string) =>
		dispatchLayout({ type: "activate", id });
	const mapTabs = (transform: (tab: CenterTab) => CenterTab) =>
		dispatchLayout({ type: "mapTabs", transform });
	return {
		...layout,
		setTabs,
		setActiveTabId,
		mapTabs,
	};
}
