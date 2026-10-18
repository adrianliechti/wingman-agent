package terminal

import (
	"errors"
	"testing"
)

type retryResizeTerminal struct {
	Terminal
	calls int
}

func (p *retryResizeTerminal) Resize(int, int) error {
	p.calls++
	if p.calls == 1 {
		return errors.New("temporary resize failure")
	}
	return nil
}

func TestResizeFailurePreservesSizeAndAllowsRetry(t *testing.T) {
	pty := &retryResizeTerminal{}
	s := &Session{pty: pty, cols: 80, rows: 24}
	if err := s.Resize(100, 30); err == nil {
		t.Fatal("missing resize failure")
	}
	if cols, rows := s.Size(); cols != 80 || rows != 24 {
		t.Errorf("failed resize changed reported size to %dx%d", cols, rows)
	}
	if err := s.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if pty.calls != 2 {
		t.Errorf("retry did not reach the terminal: %d calls", pty.calls)
	}
	if cols, rows := s.Size(); cols != 100 || rows != 30 {
		t.Errorf("successful resize reported %dx%d", cols, rows)
	}
}
