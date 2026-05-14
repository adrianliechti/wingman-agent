package code

import (
	"fmt"
	"sync"
	"time"

	"github.com/rivo/tview"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type Spinner struct {
	view     *tview.TextView
	app      *tview.Application
	ticker   *time.Ticker
	stopChan chan struct{}

	mu     sync.Mutex
	active bool
	frame  int
	phase  AppPhase
}

func NewSpinner(app *tview.Application, view *tview.TextView) *Spinner {
	return &Spinner{
		view:     view,
		app:      app,
		stopChan: make(chan struct{}),
	}
}

// Must be called from the UI goroutine (e.g. inside QueueUpdateDraw).
func (s *Spinner) Start(phase AppPhase) {
	s.mu.Lock()

	s.phase = phase
	s.frame = 0

	if s.active {
		s.render()
		s.mu.Unlock()
		return
	}

	s.active = true
	s.ticker = time.NewTicker(100 * time.Millisecond)
	s.stopChan = make(chan struct{})

	s.render()
	s.mu.Unlock()

	go s.run()
}

// Must be called from the UI goroutine (e.g. inside QueueUpdateDraw).
func (s *Spinner) Stop() {
	s.mu.Lock()

	if !s.active {
		s.mu.Unlock()
		return
	}

	s.active = false
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.stopChan)

	s.mu.Unlock()

	s.view.SetText("")
}

func (s *Spinner) run() {
	for {
		select {
		case <-s.stopChan:
			return
		case <-s.ticker.C:
			s.queueRender()
		}
	}
}

func (s *Spinner) queueRender() {
	s.app.QueueUpdateDraw(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.active {
			return
		}
		s.frame = (s.frame + 1) % len(spinnerFrames)
		s.render()
	})
}

// Caller must hold s.mu.
func (s *Spinner) render() {
	config := GetPhaseConfig(s.phase)
	if config.Message == "" {
		s.view.SetText("")
		return
	}
	frame := spinnerFrames[s.frame]
	s.view.SetText(fmt.Sprintf("[%s]%s %s[-]", config.Color, frame, config.Message))
}
