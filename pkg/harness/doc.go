// Package harness runs stateful agents against a model provider.
//
// Config describes reusable model, prompt, tool, and hook policy. Clone copies
// its hook slices; Derive also removes hooks scoped to the parent session.
// Resources and workers belong to an Agent instance and are never inherited
// through Config.
//
// An Agent owns its conversation ledger and admits one operation at a time.
// Send reserves the agent before PrepareTurn and returns a single-use stream;
// callers must consume it even after cancellation to settle the recorded turn.
// QueueInput steers an active turn. Hosts own queues of future turns, session
// persistence, workspace policy, and user interfaces.
//
// Hosts supply an io.Closer through Resources and start workers with Go after
// publishing the agent. Close rejects new work, cancels operations and workers,
// joins workers, closes Resources, and runs SessionEnd hooks.
// The host retains ownership of shared dependencies such as telemetry and the
// workspace.
package harness
