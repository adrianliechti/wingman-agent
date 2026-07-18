//go:build !windows

package inline

import (
	"os"
	"os/signal"
	"syscall"
)

func setupConsole() error {
	return nil
}

func (t *Terminal) watchResize() {
	if t.in == nil {
		return
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)

	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				t.checkResize()
			case <-t.done:
				return
			}
		}
	}()
}
