//go:build windows

package theme

// OSC 11 is not reliably supported across Windows terminals.
func queryTerminalBackground() bool {
	return false
}