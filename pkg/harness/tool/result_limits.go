package tool

// MaxInlineResultBytes is the final text-result budget, including notices.
// Hooks must persist larger results before the harness applies its fallback.
const MaxInlineResultBytes = 48 * 1024

// MaxGrepResultBytes is shared by grep's pagination and the truncation hook.
const MaxGrepResultBytes = 20 * 1024
