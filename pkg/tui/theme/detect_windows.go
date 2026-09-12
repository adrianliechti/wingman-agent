//go:build windows

package theme

func queryTerminalBackground() (light, known bool) {
	return false, false
}
