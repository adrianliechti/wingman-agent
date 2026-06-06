package jsonrpc2

import (
	"context"
	"errors"
)

var (
	ErrIdleTimeout = errors.New("timed out waiting for new connections")

	ErrNotHandled = errors.New("JSON RPC not handled")
)

type Preempter interface {
	Preempt(ctx context.Context, req *Request) (result any, err error)
}

type PreempterFunc func(ctx context.Context, req *Request) (any, error)

func (f PreempterFunc) Preempt(ctx context.Context, req *Request) (any, error) {
	return f(ctx, req)
}

var _ Preempter = PreempterFunc(nil)

type Handler interface {
	Handle(ctx context.Context, req *Request) (result any, err error)
}

type defaultHandler struct{}

func (defaultHandler) Preempt(context.Context, *Request) (any, error) {
	return nil, ErrNotHandled
}

func (defaultHandler) Handle(context.Context, *Request) (any, error) {
	return nil, ErrNotHandled
}

type HandlerFunc func(ctx context.Context, req *Request) (any, error)

func (f HandlerFunc) Handle(ctx context.Context, req *Request) (any, error) {
	return f(ctx, req)
}

var _ Handler = HandlerFunc(nil)

type async struct {
	ready    chan struct{}
	firstErr chan error
}

func newAsync() *async {
	var a async
	a.ready = make(chan struct{})
	a.firstErr = make(chan error, 1)
	a.firstErr <- nil
	return &a
}

func (a *async) done() {
	close(a.ready)
}

func (a *async) wait() error {
	<-a.ready
	err := <-a.firstErr
	a.firstErr <- err
	return err
}

func (a *async) setError(err error) {
	storedErr := <-a.firstErr
	if storedErr == nil {
		storedErr = err
	}
	a.firstErr <- storedErr
}
