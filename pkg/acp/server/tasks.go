package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
)

// taskPump forwards a session's background events — finished background
// agents, exited background commands, due scheduled tasks — to the model. It
// is the ACP counterpart of the TUI and web pumps and delivers the same hidden
// input, steered into the running turn.
//
// Unlike those front ends, the server cannot start a turn for an event that
// arrives while the session is idle: an ACP turn is the client's
// session/prompt request, and output sent outside one has no request to
// report its outcome and is dropped by clients such as Wingman's own. Such
// events are held and ride along with the client's next prompt instead.
type taskPump struct {
	id    acpsdk.SessionId
	reg   *task.Registry
	steer func(code.TurnInput) error

	quit chan struct{}
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	held    []task.Event
	stopped bool
}

// startTaskPump returns nil when the session has no task registry; every
// method accepts a nil pump.
func startTaskPump(a *agent.Agent, id acpsdk.SessionId) *taskPump {
	reg := a.Tasks(string(id))
	if reg == nil {
		return nil
	}
	return newTaskPump(id, reg, func(input code.TurnInput) error {
		return a.Steer(context.Background(), string(id), input)
	})
}

func newTaskPump(id acpsdk.SessionId, reg *task.Registry, steer func(code.TurnInput) error) *taskPump {
	p := &taskPump{
		id: id, reg: reg, steer: steer,
		quit: make(chan struct{}), done: make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *taskPump) run() {
	defer close(p.done)
	for {
		select {
		case <-p.quit:
			return
		case <-p.reg.Done():
			return
		case ev := <-p.reg.Events():
			// Events that piled up deliver as one input, not one each.
			batch := []task.Event{ev}
			for {
				select {
				case more := <-p.reg.Events():
					batch = append(batch, more)
					continue
				default:
				}
				break
			}
			p.deliver(batch)
		}
	}
}

func (p *taskPump) deliver(batch []task.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = holdTaskEvents(p.held, batch)
	p.flushLocked()
}

// flushLocked steers every held event into the running turn as one input.
// An event leaves the pump only once the agent has accepted it, so it is
// delivered exactly once; a refused batch stays held and is offered again
// with the next event and the next prompt.
func (p *taskPump) flushLocked() {
	if p.stopped || len(p.held) == 0 {
		return
	}
	err := p.steer(taskInput(p.held))
	if err == nil {
		p.held = nil
		return
	}
	if !errors.Is(err, code.ErrNoActiveTurn) {
		slog.Warn("deliver background results failed; holding them for the next prompt", "session", p.id, "err", err)
	}
}

// flush offers the held events to a turn that has just become steerable.
func (p *taskPump) flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
}

// take hands the held events to a starting prompt, which sends them with its
// input. The prompt returns them with restore if its turn does not start.
func (p *taskPump) take() []task.Event {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	held := p.held
	p.held = nil
	return held
}

// restore puts events back ahead of anything held since they were taken.
func (p *taskPump) restore(events []task.Event) {
	if p == nil || len(events) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		republishTaskEvents(p.reg, events)
		return
	}
	p.held = holdTaskEvents(slices.Clone(events), p.held)
	p.flushLocked()
}

// stop ends the pump, waits for its goroutine, and returns the events it
// could not deliver. It is safe to call more than once.
func (p *taskPump) stop() []task.Event {
	if p == nil {
		return nil
	}
	p.once.Do(func() { close(p.quit) })
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	held := p.held
	p.held = nil
	return held
}

// close stops the pump when its ACP session closes. Undelivered events return
// to the registry, which buffers them for the pump of a later session/load or
// session/resume for as long as the agent keeps the session in memory.
func (p *taskPump) close() {
	if p == nil {
		return
	}
	republishTaskEvents(p.reg, p.stop())
}

func republishTaskEvents(reg *task.Registry, events []task.Event) {
	for _, ev := range events {
		reg.Publish(ev)
	}
}

// holdTaskEvents queues batch behind held. Results are never replaced. A
// scheduled task that comes due again while its earlier trigger is still held
// supersedes that trigger: both ask for the same run, and with no turn to
// consume them a recurring schedule would grow the next prompt without bound.
func holdTaskEvents(held, batch []task.Event) []task.Event {
	for _, ev := range batch {
		if ev.Kind == task.KindSchedule {
			held = slices.DeleteFunc(held, func(old task.Event) bool {
				return old.Kind == task.KindSchedule && old.ID == ev.ID
			})
		}
		held = append(held, ev)
	}
	return held
}

// taskInput renders a batch as the one hidden steering input the TUI and web
// pumps submit, with the same identity.
func taskInput(batch []task.Event) code.TurnInput {
	first := batch[0]
	return code.TurnInput{
		ID:      fmt.Sprintf("task-%s-%d", first.ID, first.Seq),
		Intent:  code.TurnInputSteer,
		Origin:  "task",
		Content: []harness.Content{taskContent(batch)},
	}
}

func taskContent(batch []task.Event) harness.Content {
	blocks := make([]string, 0, len(batch))
	for _, ev := range batch {
		blocks = append(blocks, ev.Notification())
	}
	return harness.Content{Text: strings.Join(blocks, "\n\n"), Hidden: true}
}
