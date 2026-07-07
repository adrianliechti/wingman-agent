package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
)

type Channel struct {
	mu    sync.Mutex
	agent string

	interactive bool
}

func New() *Channel {
	return &Channel{
		agent:       "main",
		interactive: term.IsTerminal(int(os.Stdin.Fd())),
	}
}

func (c *Channel) Name() string { return "cli" }

func (c *Channel) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agent
}

func (c *Channel) setCurrent(name string) {
	c.mu.Lock()
	c.agent = name
	c.mu.Unlock()
}

func (c *Channel) Start(ctx context.Context, handler channel.MessageHandler) error {
	lines := make(chan string)

	go func() {
		defer close(lines)

		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

		for scanner.Scan() {
			lines <- scanner.Text()
		}

		if err := scanner.Err(); err != nil {
			log.Printf("cli: read stdin: %v", err)
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

			if text == "/help" {
				fmt.Println("Commands:")
				fmt.Println("  /agent [name]  show or switch the target agent")
				fmt.Println("  /exit          quit")
				c.printPrompt()
				continue
			}

			if text == "/agent" {
				fmt.Printf("Current agent: %s\n", c.current())
				c.printPrompt()
				continue
			}

			if rest, ok := strings.CutPrefix(text, "/agent "); ok {
				if name := strings.TrimSpace(rest); name != "" {
					c.setCurrent(name)
					fmt.Printf("Switched to agent: %s\n", name)
				}

				c.printPrompt()
				continue
			}

			agent := c.current()

			handler(ctx, channel.Message{
				Channel:      c.Name(),
				Conversation: agent,
				Sender:       "user",
				Agent:        agent,
				Content:      text,
			})
			c.printPrompt()
		}
	}
}

// Send delivers out-of-band messages (scheduled reports); direct replies
// stream through SendStream instead.
func (c *Channel) Send(ctx context.Context, conversation string, text string) error {
	fmt.Printf("\n[%s] %s\n", conversation, text)
	c.printPrompt()
	return nil
}

func (c *Channel) SendStream(ctx context.Context, conversation string) (io.WriteCloser, error) {
	return &streamWriter{interactive: c.interactive}, nil
}

func (c *Channel) printPrompt() {
	if !c.interactive {
		return
	}

	fmt.Printf("[%s] > ", c.current())
}

type streamWriter struct {
	interactive bool
	wrote       bool
	last        byte
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if !w.wrote && w.interactive {
		os.Stdout.Write([]byte("\n"))
	}

	w.wrote = true
	w.last = p[len(p)-1]

	return os.Stdout.Write(p)
}

func (w *streamWriter) Close() error {
	if !w.wrote {
		return nil
	}

	if w.last != '\n' {
		fmt.Println()
	}

	if w.interactive {
		fmt.Println()
	}

	return nil
}
