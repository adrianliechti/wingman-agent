// Package watch coalesces workspace change-check requests. Instead of
// polling on a fixed interval, callers Notify the monitor when something may
// have changed (a tool finished, a turn ended, the UI regained focus); a
// slow fallback tick covers changes no signal announces.
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
	// MinInterval throttles Notify-driven checks (default 2s).
	MinInterval time.Duration
	// Fallback is the idle re-check interval (default 30s).
	Fallback time.Duration
	// Active gates checks; when it returns false, signals are dropped.
	Active func() bool
}

// Monitor runs checks in a single goroutine, so checks never overlap and
// queued signals coalesce into one run.
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

// Notify requests a check soon, throttled to at most one per MinInterval.
func (m *Monitor) Notify() {
	select {
	case m.notifies <- struct{}{}:
	default:
	}
}

// Flush requests an immediate check, bypassing the throttle.
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

		// Drain signals (and a stale fallback tick) queued behind the one
		// we're acting on, so they coalesce instead of double-checking.
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
