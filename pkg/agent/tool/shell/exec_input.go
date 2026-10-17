package shell

import (
	"context"
	"fmt"
	"io"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func validateExecArgs(args map[string]any, strings, bools []string) error {
	for _, key := range strings {
		if value, present := args[key]; present && value != nil {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s must be a string", key)
			}
		}
	}
	for _, key := range bools {
		if value, present := args[key]; present && value != nil {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s must be a boolean", key)
			}
		}
	}
	return nil
}

// Input and EOF share a cancellable lock so no call can submit another call's
// pending input between its approval check and delivery.
func (s *execSession) lockInput(ctx context.Context) error {
	s.inputOnce.Do(func() { s.inputGate = make(chan struct{}, 1) })
	select {
	case s.inputGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlockInput()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *execSession) unlockInput() { <-s.inputGate }

// A cancelled write may have already sent a prefix. Terminate the process and
// close stdin to unblock the writer and prevent deferred execution of that
// prefix. Cancellation of ordinary polling leaves the process running.
func (s *execSession) writeInput(ctx context.Context, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.cancel()
		s.stdin.Close()
		close(closed)
	})
	n, err := io.WriteString(s.stdin, input)
	if !stop() {
		<-closed
		return fmt.Errorf("stdin write cancelled; session %d termination requested: %w", s.id, ctx.Err())
	}
	if err == nil && n != len(input) {
		err = io.ErrShortWrite
	}
	return err
}

func applySessionInput(ctx context.Context, m *ExecManager, s *execSession, elicit *tool.Elicitation, input string, eof bool) (*tool.Result, error) {
	if input == "" && !eof {
		return nil, nil
	}
	if err := s.lockInput(ctx); err != nil {
		return nil, err
	}
	defer s.unlockInput()

	if input == "\x03" {
		if !s.tty {
			if err := s.interrupt(); err != nil && !s.exited() {
				return nil, fmt.Errorf("cannot interrupt on this platform; use kill instead: %w", err)
			}
		} else if err := s.writeInput(ctx, input); err != nil && !s.exited() {
			return nil, fmt.Errorf("failed to write interrupt to stdin: %w", err)
		}
		s.pendingInput = ""
		s.inputOverflow = false
		// A signal does not flush bytes already written into a pipe.
		s.inputUncertain = !s.tty
	} else if input != "" {
		if result, err := writeSessionInputLocked(ctx, m, s.id, s, elicit, input); result != nil || err != nil {
			return result, err
		}
	}
	if eof {
		if err := confirmPendingSessionInputLocked(ctx, s, elicit); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.tty {
			if err := s.writeInput(ctx, "\x04"); err != nil && !s.exited() {
				return nil, fmt.Errorf("failed to send EOF to terminal: %w", err)
			}
			// Readline can treat Ctrl-D as delete-char on a nonempty line.
			s.inputUncertain = true
		} else if err := s.stdin.Close(); err != nil && !s.exited() {
			return nil, fmt.Errorf("failed to close stdin: %w", err)
		}
	}
	return nil, nil
}
