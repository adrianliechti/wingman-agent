package watch

import (
	"context"
	"time"
)

const (
	defaultMinInterval = 2 * time.Second
	defaultFallback    = 30 * time.Second
)

type Options struct {
	MinInterval time.Duration

	Fallback time.Duration

	Active func() bool
}

type Monitor struct {
	check func()
	opts  Options

	notifies chan struct{}
	urgent   chan struct{}
}

func New(opts Options, check func()) *Monitor {
	if opts.MinInterval <= 0 {
		opts.MinInterval = defaultMinInterval
	}
	if opts.Fallback <= 0 {
		opts.Fallback = defaultFallback
	}
	return &Monitor{
		check:    check,
		opts:     opts,
		notifies: make(chan struct{}, 1),
		urgent:   make(chan struct{}, 1),
	}
}

func (m *Monitor) Notify() {
	select {
	case m.notifies <- struct{}{}:
	default:
	}
}

func (m *Monitor) Flush() {
	select {
	case m.urgent <- struct{}{}:
	default:
	}
}

func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.opts.Fallback)
	defer ticker.Stop()

	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.urgent:
		case <-m.notifies:
			if wait := m.opts.MinInterval - time.Since(last); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-m.urgent:
					timer.Stop()
				case <-timer.C:
				}
			}
		case <-ticker.C:
		}

		select {
		case <-m.notifies:
		default:
		}
		select {
		case <-m.urgent:
		default:
		}
		select {
		case <-ticker.C:
		default:
		}

		if m.opts.Active != nil && !m.opts.Active() {
			continue
		}
		m.check()
		last = time.Now()
	}
}
