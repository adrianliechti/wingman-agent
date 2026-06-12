package channel

import (
	"context"
	"io"
)

type Message struct {
	// Channel is the name of the channel that received the message.
	Channel string
	// Conversation is the platform-specific conversation address replies go to.
	Conversation string
	// Sender identifies the platform user who sent the message; empty if unknown.
	Sender string
	// Agent is the target agent resolved by the channel.
	Agent string

	Content string
}

type Route struct {
	Channel      string
	Conversation string
}

type MessageHandler func(ctx context.Context, msg Message)

type Channel interface {
	Name() string

	Start(ctx context.Context, handler MessageHandler) error

	// Send delivers a complete message to a conversation. Implementations
	// chunk to their platform limits as needed.
	Send(ctx context.Context, conversation string, text string) error
}

// Streamer is an optional capability for channels that can render
// incremental output (terminals). Others receive one Send per turn.
type Streamer interface {
	SendStream(ctx context.Context, conversation string) (io.WriteCloser, error)
}
