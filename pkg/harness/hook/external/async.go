package external

import (
	"context"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
)

// Async runs observe-only handlers in the background and delivers their context when they finish.
type Async struct {
	deliver func(runtime hook.Runtime, parts []string)

	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{}

	mu sync.Mutex // Serializes worker admission with cancellation before Wait.
	wg sync.WaitGroup
}

func NewAsync(deliver func(runtime hook.Runtime, parts []string)) *Async {
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
	if a.ctx.Err() != nil {
		return
	}
	// Async hooks are best-effort: a full pool drops the run instead of parking goroutines.
	select {
	case a.slots <- struct{}{}:
	default:
		return
	}
	a.wg.Go(func() {
		defer func() { <-a.slots }()
		parts := run(hook.WithRuntime(a.ctx, runtime))
		if len(parts) == 0 || a.ctx.Err() != nil || a.deliver == nil {
			return
		}
		a.deliver(runtime, parts)
	})
}

func (a *Async) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.cancel()
	a.mu.Unlock()
	a.wg.Wait()
}
