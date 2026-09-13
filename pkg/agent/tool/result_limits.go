package tool

// MaxInlineResultBytes is the final text-result budget, including notices.
// Hooks must persist larger results before the harness applies its fallback.
const MaxInlineResultBytes = 48 * 1024
