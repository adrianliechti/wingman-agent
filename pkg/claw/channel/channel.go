package channel

import (
	"context"
	"io"
	"time"
)

type Message struct {
	ChatID    string
	Sender    string
	Content   string
	Timestamp time.Time
	IsBot     bool
}

type MessageHandler func(ctx context.Context, msg Message)

type Channel interface {
	Name() string

	// Start blocks until the context is cancelled or the channel is exhausted.
	Start(ctx context.Context, handler MessageHandler) error

	Send(ctx context.Context, chatID string, text string) error

	// SendStream returns a writer for partial output. Callers must close it when done.
	SendStream(ctx context.Context, chatID string) (io.WriteCloser, error)
}
