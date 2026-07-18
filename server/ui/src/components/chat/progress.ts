import { createContext } from "react";

// Live status text from running tools, keyed by tool-call ID. A context so
// updates reach the running ToolRow without invalidating the memoized turns.
export const ToolProgressContext = createContext<Record<string, string>>({});
