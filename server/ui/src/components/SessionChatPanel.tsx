import { useEffect, useMemo } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { isDraft } from "../state/sessionStore";
import { listTurnReviews, turnReviewKey } from "../api/turnReviews";
import { TurnReviewsContext } from "./chat/turnReviewContext";
import { useSessionView } from "../hooks/useSessionView";
import { ChatPanel, type ChatPanelProps } from "./ChatPanel";
const EMPTY: never[] = [];
// A mounted chat subscribes to its own transcript. The workspace layout only
// observes summaries, so tokens in another session do not rerender its panes.
export function SessionChatPanel(
	props: Omit<
		ChatPanelProps,
		| "entries"
		| "phase"
		| "pendingInputs"
		| "queuePaused"
		| "canSteer"
		| "prompts"
		| "toolProgress"
	>,
) {
	const view = useSessionView(props.sessionId);
	const session = props.sessionId ?? "";
	const client = useQueryClient();
	const query = useQuery({
		queryKey: turnReviewKey(session),
		enabled: !!session && !isDraft(session) && view?.status === "ready",
		queryFn: ({ signal }) => listTurnReviews(session, signal),
	});
	useEffect(() => {
		// A fast turn can enter and leave its active phase between renders.
		// Refresh for the settled revision even when React only observed idle.
		if (view?.phase === "idle")
			void client.invalidateQueries({ queryKey: turnReviewKey(session) });
	}, [client, session, view?.phase, view?.revision]);
	const context = useMemo(
		() => ({
			session,
			reviews: new Map(
				(query.data ?? []).map((review) => [review.inputId, review]),
			),
			available: props.available ?? false,
			active: (view?.phase ?? "idle") !== "idle",
		}),
		[session, query.data, props.available, view?.phase],
	);
	return (
		<TurnReviewsContext.Provider value={context}>
			<ChatPanel
				{...props}
				entries={view?.entries ?? EMPTY}
				phase={view?.phase ?? "idle"}
				retry={view?.retry}
				pendingInputs={view?.pendingInputs ?? EMPTY}
				queuePaused={view?.queuePaused ?? false}
				canSteer={view?.canSteer ?? false}
				prompts={view?.prompts ?? EMPTY}
				toolProgress={view?.toolProgress}
				incomplete={
					!query.isFetching && query.data?.at(-1)?.outcome === "incomplete"
				}
			/>
		</TurnReviewsContext.Provider>
	);
}
