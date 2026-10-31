import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { isDraft } from "../state/sessionStore";
import type { WorkspaceClient } from "../state/workspaceClient";
import { listTurnReviews, turnReviewKey } from "../api/turnReviews";
import { TurnReviewsContext } from "./chat/turnReviewContext";
import { useSessionView } from "../hooks/useSessionView";
import { ChatPanel, type ChatPanelProps } from "./ChatPanel";
const EMPTY: never[] = [];
// A mounted chat subscribes to its own transcript. The workspace layout only
// observes summaries, so tokens in another session do not rerender its panes.
export function SessionChatPanel({
	onAct,
	...props
}: Omit<
	ChatPanelProps,
	| "entries"
	| "phase"
	| "pendingInputs"
	| "prompts"
	| "activity"
	| "incomplete"
	| "onCancel"
	| "onCompact"
	| "onRemoveQueued"
	| "onUpdateQueued"
	| "onClearQueue"
	| "onPromptReply"
> & {
	onAct: (
		operation: (client: WorkspaceClient) => Promise<unknown>,
	) => Promise<boolean>;
}) {
	const view = useSessionView(props.sessionId);
	const session = props.sessionId ?? "";
	const act = (operation: (client: WorkspaceClient) => Promise<unknown>) =>
		session ? onAct(operation) : Promise.resolve(false);
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
	const context = {
		session,
		reviews: new Map(
			(query.data ?? []).map((review) => [review.inputId, review]),
		),
		available: props.available ?? false,
		active: (view?.phase ?? "idle") !== "idle",
	};
	return (
		<TurnReviewsContext value={context}>
			<ChatPanel
				{...props}
				onCancel={() => {
					void act((client) => client.cancel(session));
				}}
				onCompact={(text) => act((client) => client.compact(session, text))}
				onRemoveQueued={(inputId, state) => {
					if (state === "queued" || state === "sending")
						void act((client) => client.removeQueued(session, inputId));
				}}
				onUpdateQueued={(inputId, text, files, images, completions) =>
					act((client) =>
						client.updateQueued(
							session,
							inputId,
							text,
							files,
							images,
							completions,
						),
					)
				}
				onClearQueue={() => {
					void act((client) => client.clearQueue(session));
				}}
				onPromptReply={(promptId, reply) => {
					void act((client) => client.respond(session, promptId, reply));
				}}
				entries={view?.entries ?? EMPTY}
				phase={view?.phase ?? "idle"}
				activity={view?.activity}
				pendingInputs={view?.pendingInputs ?? EMPTY}
				prompts={view?.prompts ?? EMPTY}
				incomplete={
					!query.isFetching && query.data?.at(-1)?.outcome === "incomplete"
				}
			/>
		</TurnReviewsContext>
	);
}
