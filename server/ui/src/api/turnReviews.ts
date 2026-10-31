import { fetchJSON, fetchOK } from "./http.ts";
import { chatPath } from "../state/workspaceClient.ts";
export type TurnFileChange = {
	path: string;
	before: string;
	after: string;
	beforeExists: boolean;
	afterExists: boolean;
};
export type TurnReview = {
	id: string;
	inputId: string;
	started: string;
	outcome: string;
	files: TurnFileChange[];
	checks: {
		command: string;
		workdir?: string;
		outcome: string;
		exitCode?: number;
	}[];
	validation: string;
	untracked: boolean;
	uncertain: boolean;
	undone: boolean;
	// Net line counts; only review listings carry them.
	added?: number;
	removed?: number;
};
export const turnReviewKey = (session: string) =>
	["server", "turn-reviews", session] as const;
export const listTurnReviews = (session: string, signal?: AbortSignal) =>
	fetchJSON<TurnReview[]>(`${chatPath(session)}/reviews`, { signal });
export const getTurnReview = (
	session: string,
	turn: string,
	signal?: AbortSignal,
) =>
	fetchJSON<TurnReview>(
		`${chatPath(session)}/reviews/${encodeURIComponent(turn)}`,
		{ signal },
	);
export const undoTurn = (session: string, turn: string) =>
	fetchOK(`${chatPath(session)}/reviews/${encodeURIComponent(turn)}/undo`, {
		method: "POST",
	});
