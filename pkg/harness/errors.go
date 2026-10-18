package harness

import (
	"context"
	"errors"
	"fmt"
)

var ErrMaxTurnsExceeded = errors.New("agent: internal turn-loop safety bound exceeded — likely a runaway tool-call cycle")

// ErrTurnIncomplete means the model stopped without confirming completion.
// The transcript and completed tool work are preserved; fresh input can resume it.
var ErrTurnIncomplete = errors.New("agent: turn incomplete")

// ErrInterrupted is the cancel cause of a user interrupt; only it fires Interrupt hooks.
var ErrInterrupted = fmt.Errorf("interrupted by the user: %w", context.Canceled)

// ErrNothingToCompact means a manual compaction found no conversation to summarize.
var ErrNothingToCompact = errors.New("nothing to compact yet")
