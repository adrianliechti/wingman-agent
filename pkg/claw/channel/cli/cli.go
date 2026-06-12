package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
)

type Channel struct {
	agent string
}

func New() *Channel {
	return &Channel{agent: "main"}
}

func (c *Channel) Name() string { return "cli" }

func (c *Channel) Start(ctx context.Context, handler channel.MessageHandler) error {
	lines := make(chan string)

	go func() {
		defer close(lines)

		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	c.printPrompt()

	for {
		select {
		case <-ctx.Done():
			return nil

		case line, ok := <-lines:
			if !ok {
				return nil
			}

			text := strings.TrimSpace(line)
			if text == "" {
				c.printPrompt()
				continue
			}

			if text == "/exit" || text == "/quit" {
				return nil
			}

			if rest, ok := strings.CutPrefix(text, "/agent "); ok {
				if name := strings.TrimSpace(rest); name != "" {
					c.agent = name
					fmt.Printf("Switched to agent: %s\n", name)
				}

				c.printPrompt()
				continue
			}

			if text == "/agent" {
				fmt.Printf("Current agent: %s\n", c.agent)
				c.printPrompt()
				continue
			}

			handler(ctx, channel.Message{
				Channel:      c.Name(),
				Conversation: c.agent,
				Sender:       "user",
				Agent:        c.agent,
				Content:      text,
			})
			c.printPrompt()
		}
	}
}

func (c *Channel) Send(ctx context.Context, conversation string, text string) error {
	fmt.Println(text)
	return nil
}

func (c *Channel) SendStream(ctx context.Context, conversation string) (io.WriteCloser, error) {
	return &streamWriter{}, nil
}

func (c *Channel) printPrompt() {
	fmt.Printf("[%s] > ", c.agent)
}

type streamWriter struct{}

func (w *streamWriter) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (w *streamWriter) Close() error {
	fmt.Println()
	return nil
}
