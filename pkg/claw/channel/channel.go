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

	Start(ctx context.Context, handler MessageHandler) error

	Send(ctx context.Context, chatID string, text string) error

	SendStream(ctx context.Context, chatID string) (io.WriteCloser, error)
}
