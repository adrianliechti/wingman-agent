package agent

import "errors"

var ErrMaxTurnsExceeded = errors.New("agent: internal turn-loop safety bound exceeded — likely a runaway tool-call cycle")

// ErrTurnIncomplete means the model stopped without confirming completion.
// The transcript and completed tool work are preserved; fresh input can resume it.
var ErrTurnIncomplete = errors.New("agent: turn incomplete")
