import { createContext } from "react";
import type { TurnReview } from "../../api/turnReviews";
export const TurnReviewsContext = createContext<{
	session: string;
	reviews: Map<string, TurnReview>;
	available: boolean;
	active: boolean;
}>({ session: "", reviews: new Map(), available: false, active: false });
