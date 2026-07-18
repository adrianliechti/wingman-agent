//go:build windows

package inline

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func setupConsole() error {
	out := windows.Handle(os.Stdout.Fd())

	var mode uint32
	if err := windows.GetConsoleMode(out, &mode); err != nil {
		return nil
	}
	windows.SetConsoleMode(out, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)

	in := windows.Handle(os.Stdin.Fd())
	if err := windows.GetConsoleMode(in, &mode); err == nil {
		windows.SetConsoleMode(in, mode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	}

	return nil
}

func (t *Terminal) watchResize() {
	if t.in == nil {
		return
	}

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.checkResize()
			case <-t.done:
				return
			}
		}
	}()
}
