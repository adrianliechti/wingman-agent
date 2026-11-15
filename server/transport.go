package server

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	wsWriteTimeout = 5 * time.Second
	wsOutboxBuffer = 256
)

type wsClient struct {
	conn   *websocket.Conn
	outbox chan []byte

	mu     sync.Mutex
	closed bool
}

func newWSClient(conn *websocket.Conn) *wsClient {
	return &wsClient{
		conn:   conn,
		outbox: make(chan []byte, wsOutboxBuffer),
	}
}

func (c *wsClient) enqueue(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.outbox <- data:
		return true
	default:
		return false
	}
}

func (c *wsClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.outbox)
}

func (c *wsClient) run() {
	for data := range c.outbox {
		ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
		err := c.conn.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			_ = c.conn.CloseNow()

			for range c.outbox {
			}
			return
		}
	}
}
