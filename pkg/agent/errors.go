package agent

import "errors"

var ErrMaxTurnsExceeded = errors.New("agent: internal turn-loop safety bound exceeded — likely a runaway tool-call cycle")
