import { fetchJSON, fetchOK } from "./http.ts";
import { chatPath } from "../state/workspaceClient.ts";
// Both carry one net change per file: from the version the agent first found to its last.
type TurnFile = { path: string; beforeExists: boolean; afterExists: boolean };
export type TurnFileStats = TurnFile & { added: number; removed: number };
export type TurnFileChange = TurnFile & { before: string; after: string };
type Review<File> = {
	id: string;
	inputId: string;
	started: string;
	outcome: string;
	files: File[];
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
};
export type TurnReview = Review<TurnFileStats>;
export type TurnReviewDetail = Review<TurnFileChange>;
export const turnReviewKey = (session: string) =>
	["server", "turn-reviews", session] as const;
export const listTurnReviews = (session: string, signal?: AbortSignal) =>
	fetchJSON<TurnReview[]>(`${chatPath(session)}/reviews`, { signal });
export const getTurnReview = (
	session: string,
	turn: string,
	signal?: AbortSignal,
) =>
	fetchJSON<TurnReviewDetail>(
		`${chatPath(session)}/reviews/${encodeURIComponent(turn)}`,
		{ signal },
	);
export const undoTurn = (session: string, turn: string) =>
	fetchOK(`${chatPath(session)}/reviews/${encodeURIComponent(turn)}/undo`, {
		method: "POST",
	});
