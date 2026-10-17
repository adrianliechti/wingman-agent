//go:build !windows

package theme

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

func queryTerminalBackground() (light, known bool) {
	fd := int(os.Stdin.Fd())

	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false, false
	}

	oldState, err := term.MakeRaw(fd)

	if err != nil {
		return false, false
	}
	defer term.Restore(fd, oldState)

	if _, err := os.Stdout.WriteString("\x1b]11;?\x07"); err != nil {
		return false, false
	}
	return readTerminalBackground(fd, 200*time.Millisecond)
}

// TTY files can reject os.File.SetReadDeadline (including on macOS). Poll
// readiness directly, with one deadline for the entire fragmented reply and
// no background reader left consuming input after a timeout.
func readTerminalBackground(fd int, timeout time.Duration) (light, known bool) {
	deadline := time.Now().Add(timeout)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var response strings.Builder
	var buf [128]byte
	for response.Len() < 4096 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, false
		}
		milliseconds := int((remaining + time.Millisecond - 1) / time.Millisecond)
		n, err := unix.Poll(fds, milliseconds)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 || fds[0].Revents&unix.POLLIN == 0 {
			return false, false
		}
		n, err = unix.Read(fd, buf[:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			return false, false
		}
		response.Write(buf[:n])
		if color, ok := parseBackgroundColor(response.String()); ok {
			luma := (0.299*float64(color.R) + 0.587*float64(color.G) + 0.114*float64(color.B)) / 255
			return luma > 0.5, true
		}
	}
	return false, false
}

// An OSC reply is valid only once BEL or ST has terminated all three
// components; a partial reply must never select the wrong palette.
func parseBackgroundColor(s string) (ansi.Color, bool) {
	_, s, found := strings.Cut(s, "\x1b]11;rgb:")
	if !found {
		return ansi.Color{}, false
	}
	end := strings.IndexByte(s, '\a')
	if st := strings.Index(s, "\x1b\\"); st >= 0 && (end < 0 || st < end) {
		end = st
	}
	if end < 0 {
		return ansi.Color{}, false
	}
	parts := strings.Split(s[:end], "/")
	if len(parts) != 3 {
		return ansi.Color{}, false
	}
	var rgb [3]uint8
	for i, part := range parts {
		if len(part) < 1 || len(part) > 4 {
			return ansi.Color{}, false
		}
		value, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return ansi.Color{}, false
		}
		rgb[i] = uint8(value * 255 / ((1 << (4 * len(part))) - 1))
	}
	return ansi.Color{R: rgb[0], G: rgb[1], B: rgb[2]}, true
}
