package external

import (
	"context"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
)

// Async runs observe-only handlers in the background and delivers their context when they finish.
type Async struct {
	deliver func(runtime hook.Runtime, context []string)

	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{}

	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
}

func NewAsync(deliver func(runtime hook.Runtime, context []string)) *Async {
	ctx, cancel := context.WithCancel(context.Background())
	return &Async{
		deliver: deliver,
		ctx:     ctx,
		cancel:  cancel,
		slots:   make(chan struct{}, maxConcurrentAsyncHooks),
	}
}

func (a *Async) start(runtime hook.Runtime, run func(ctx context.Context) []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	// Async hooks are best-effort: a full pool drops the run instead of parking goroutines.
	select {
	case a.slots <- struct{}{}:
	default:
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() { <-a.slots }()
		context := run(hook.WithRuntime(a.ctx, runtime))
		if len(context) == 0 || a.ctx.Err() != nil || a.deliver == nil {
			return
		}
		a.deliver(runtime, context)
	}()
}

func (a *Async) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.cancel()
	a.wg.Wait()
}
