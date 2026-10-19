package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusStopped Status = "stopped"
)

const (
	// MaxConcurrent guards provider rate limits, not local resources — the
	// runs are IO-bound model streams, so CPU count is irrelevant.
	MaxConcurrent = 16
	MaxPerSession = 50

	// MaxRunDuration bounds one background run so a hung model stream cannot
	// occupy a concurrency slot forever.
	MaxRunDuration = 30 * time.Minute
)

type Task struct {
	ID          string
	AgentID     string
	Description string
	AgentType   string
	Started     time.Time

	mu       sync.Mutex
	status   Status
	result   string
	finished time.Time
	stopped  bool
	cancel   context.CancelFunc
	activity string
	seq      int
	peek     func() []harness.Message
	resume   func(ctx context.Context, prompt string) error
	registry *Registry

	finishing bool // Prevent relaunch while the terminal result is being saved.

	agentState harness.State
	resumeData json.RawMessage
}

// Info is one consistent view of a task. Reading status, result and sequence
// separately could combine fields from different runs during a relaunch.
type Info struct {
	ID          string
	Description string
	AgentType   string
	Status      Status
	Result      string
	Activity    string
	Elapsed     time.Duration
	Seq         int
}

func (t *Task) Info() Info {
	t.mu.Lock()
	defer t.mu.Unlock()
	info := Info{
		ID: t.ID, Description: t.Description, AgentType: t.AgentType,
		Status: t.status, Result: t.result, Activity: t.activity, Seq: t.seq,
	}
	if t.finishing {
		info.Status = StatusRunning
	}
	if info.Status == StatusRunning {
		info.Result = ""
	}
	if t.finished.IsZero() {
		info.Elapsed = time.Since(t.Started)
	} else {
		info.Elapsed = t.finished.Sub(t.Started)
	}
	return info
}

// Seq counts the task's runs: 1 for the initial launch, +1 per resume. It
// distinguishes the completion notifications of successive runs.
func (t *Task) Seq() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seq
}

// SetResume installs the follow-up hook used by agent_send: it restarts the
// finished agent with a new prompt and its full prior context.
func (t *Task) SetResume(fn func(prompt string) error) {
	if fn == nil {
		t.SetResumeContext(nil)
		return
	}
	t.SetResumeContext(func(_ context.Context, prompt string) error { return fn(prompt) })
}

func (t *Task) SetResumeContext(fn func(context.Context, string) error) {
	t.mu.Lock()
	t.resume = fn
	t.mu.Unlock()
}

// SetDurableAgent installs the child's current event ledger and opaque resume
// specification for its admitted identity. The subagent package owns the format;
// Registry only guarantees atomic persistence.
func (t *Task) SetDurableAgent(agentID string, state harness.State, resumeData json.RawMessage) error {
	t.mu.Lock()
	if agentID != t.AgentID {
		t.mu.Unlock()
		return fmt.Errorf("agent identity %q does not match admitted identity %q", agentID, t.AgentID)
	}
	t.agentState = cloneAgentState(state)
	t.resumeData = append(json.RawMessage(nil), resumeData...)
	registry := t.registry
	t.mu.Unlock()
	if registry != nil {
		if err := registry.saveAgentState(t.ID, state); err != nil {
			return err
		}
		return registry.persist()
	}
	return nil
}

func (t *Task) DurableAgent() (string, harness.State, json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.AgentID, cloneAgentState(t.agentState), append(json.RawMessage(nil), t.resumeData...)
}

// AppendEvents implements harness.EventRecorder for the child agent. Events
// have already been assigned monotonically by that child Agent.
func (t *Task) AppendEvents(events []harness.RuntimeEvent) error {
	if len(events) == 0 {
		return nil
	}
	t.mu.Lock()
	last := uint64(0)
	if existing := t.agentState.Events; len(existing) > 0 {
		last = existing[len(existing)-1].Sequence
	}
	for _, event := range events {
		if event.Sequence != last+1 {
			t.mu.Unlock()
			return fmt.Errorf("child event sequence %d is not the next sequence %d", event.Sequence, last+1)
		}
		last = event.Sequence
	}
	registry := t.registry
	t.mu.Unlock()
	if registry != nil {
		if err := registry.appendAgentEvents(t.ID, events); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.agentState.Events = append(t.agentState.Events, events...)
	t.mu.Unlock()
	return nil
}

func (t *Task) SetAgentState(state harness.State) error {
	registry := t.registry
	if registry != nil {
		if err := registry.saveAgentState(t.ID, state); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.agentState = cloneAgentState(state)
	t.mu.Unlock()
	return nil
}

func (t *Task) Resume(prompt string) error {
	return t.ResumeContext(context.Background(), prompt)
}

func (t *Task) ResumeContext(ctx context.Context, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	if t.status == StatusRunning || t.finishing {
		t.mu.Unlock()
		return fmt.Errorf("agent %s is still running; its result arrives as a task notification", t.ID)
	}
	fn := t.resume
	t.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("agent %s does not support follow-ups", t.ID)
	}
	return fn(ctx, prompt)
}

func (t *Task) SupportsResume() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resume != nil
}

// SetActivity records what the agent is currently doing (e.g. its running
// tool call) for live status displays.
func (t *Task) SetActivity(text string) {
	t.mu.Lock()
	t.activity = text
	t.mu.Unlock()
	// Transient activity is read live and included in the next durable task
	// transition. A status update must not rewrite/fsync the whole registry.
}

func (t *Task) Activity() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.activity
}

// SetPeek installs a snapshot function for the agent's transcript so UIs can
// watch a running task and re-read a finished one.
func (t *Task) SetPeek(fn func() []harness.Message) {
	t.mu.Lock()
	t.peek = fn
	t.mu.Unlock()
}

func (t *Task) PeekMessages() []harness.Message {
	t.mu.Lock()
	fn := t.peek
	t.mu.Unlock()
	if fn == nil {
		t.mu.Lock()
		state := cloneAgentState(t.agentState)
		t.mu.Unlock()
		var restored harness.Agent
		if err := restored.Restore(state); err != nil {
			return nil
		}
		return restored.MessagesSnapshot()
	}
	return fn()
}

// KindCommand marks events published for backgrounded shell commands rather
// than subagent runs; those have no Task in the registry.
const KindCommand = "command"

// KindSchedule marks a scheduled task coming due. Unlike the other kinds it
// carries work to do rather than a finished result: Description holds the
// schedule expression and Result the task prompt.
const KindSchedule = "schedule"

// Event is an immutable snapshot of one finished run, taken before the task
// can be resumed — delivery must never read the live task, which a relaunch
// may already have reset.
type Event struct {
	Task        *Task
	ID          string
	Kind        string
	Description string
	AgentType   string
	Seq         int
	Status      Status
	Result      string
	Elapsed     time.Duration
}

// Label names the event source for status lines and notifications.
func (e Event) Label() string {
	switch e.Kind {
	case KindCommand:
		return "Background command"
	case KindSchedule:
		return "Scheduled task"
	default:
		return "Background agent"
	}
}

// Verb describes how the run ended, for status lines: "finished", "replied"
// (a resumed run), "failed", or "was stopped" — and "is due" for a schedule,
// which announces work rather than reporting it.
func (e Event) Verb() string {
	if e.Kind == KindSchedule {
		return "is due"
	}
	switch e.Status {
	case StatusFailed:
		return "failed"
	case StatusStopped:
		return "was stopped"
	default:
		if e.Seq > 1 {
			return "replied"
		}
		return "finished"
	}
}

// Notice renders the one-line user-facing announcement shown in the chat when
// the event is delivered.
func (e Event) Notice() string {
	if e.Kind == KindSchedule {
		return fmt.Sprintf("%s %s %s (%s)", e.Label(), e.ID, e.Verb(), e.Description)
	}
	return fmt.Sprintf("%s %s %s (%s, %s)", e.Label(), e.ID, e.Verb(), e.Description, e.Elapsed.Round(time.Second))
}

// Notification renders the model-facing completion block delivered as hidden
// context by every UI surface.
func (e Event) Notification() string {
	if e.Kind == KindSchedule {
		return fmt.Sprintf(
			"<scheduled-task>\nScheduled task %s (%s) is due.\nThis is an automated trigger, not user input — no human has reviewed or approved anything since the last real user message.\nRun it now, then report only what needs the user's attention.\n\nTask:\n%s\n</scheduled-task>",
			e.ID, e.Description, e.Result,
		)
	}

	return fmt.Sprintf(
		"<task-notification>\n%s %s (%s: %s) %s after %s.\nThis is an automated notification, not user input — no human has reviewed or approved anything since the last real user message.\nThe user cannot see this result. Use it to continue your work and relay what matters in your response.\n\nResult:\n%s\n</task-notification>",
		e.Label(), e.ID, e.AgentType, e.Description, e.Verb(), e.Elapsed.Round(time.Second), e.Result,
	)
}

func (t *Task) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finishing {
		return StatusRunning
	}
	return t.status
}

// Result returns the agent's final report; empty while the task is running.
func (t *Task) Result() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result
}

func (t *Task) Elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished.IsZero() {
		return time.Since(t.Started)
	}
	return t.finished.Sub(t.Started)
}

// Registry tracks the subagents of one session. Completed tasks stay listed
// for agent_output until the session ends; background completion events are
// buffered so a consumer that attaches late still receives them.
type Registry struct {
	ctx    context.Context // Cancellation is the registry's closed state.
	cancel context.CancelFunc

	events chan Event

	mu       sync.Mutex
	tasks    []*Task
	running  int
	launched int

	path          string
	persistMu     sync.Mutex
	persistGate   sync.RWMutex
	agentJournals sync.Map
}

func NewRegistry() *Registry {
	return newRegistry("")
}

func newRegistry(path string) *Registry {
	// Background tasks deliberately do not inherit the foreground turn's
	// progress or stream lifecycle callbacks: their output is represented by
	// Task activity/results, and sub-agent retries install their own local reset
	// handler around the buffered run.
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		ctx:    ctx,
		cancel: cancel,
		events: make(chan Event, MaxPerSession),
		path:   path,
	}
}

// Events delivers one snapshot per finished background run, plus published events.
func (r *Registry) Events() <-chan Event { return r.events }

// Publish delivers an external background event (e.g. an exec exit)
// through the session's notification channel alongside subagent completions.
func (r *Registry) Publish(ev Event) {
	if r.ctx.Err() != nil {
		return
	}
	r.send(ev)
}

// send delivers ev, evicting the oldest buffered event rather than dropping
// the newest when the channel is full.
func (r *Registry) send(ev Event) {
	for {
		select {
		case r.events <- ev:
			return
		default:
			select {
			case <-r.events:
			default:
			}
		}
	}
}

// Done is closed when the registry shuts down, so event consumers can exit.
func (r *Registry) Done() <-chan struct{} { return r.ctx.Done() }

// Launch starts run in a goroutine detached from the launching tool call; it
// is canceled only by Stop or Close. The returned error reports cap or
// shutdown rejections, never run failures — those surface via the task. run
// receives the task so it can publish activity and a transcript peek.
func (r *Registry) Launch(description, agentType string, run func(ctx context.Context, t *Task) (string, error)) (*Task, error) {
	return r.LaunchAgent(uuid.NewString(), description, agentType, nil, run)
}

// LaunchAgent is Launch with a stable child identity and a preparation step
// that is persisted before the goroutine can start.
func (r *Registry) LaunchAgent(agentID, description, agentType string, prepare func(*Task) error, run func(ctx context.Context, t *Task) (string, error)) (*Task, error) {
	ctx, cancel := context.WithTimeout(r.ctx, MaxRunDuration)
	t, err := r.start(agentID, description, agentType, cancel, true, prepare)
	if err != nil {
		cancel()
		return nil, err
	}
	ctx = tool.WithBackgroundOrigin(ctx, t.ID)
	go r.execute(ctx, cancel, t, true, run)
	return t, nil
}

// RunAgent tracks a synchronous call from its start, including activity,
// cancellation and durable history. Its result is returned inline, so it emits
// no completion notification. At the session limit it runs without tracking,
// preserving synchronous calls as a fallback when background work is capped.
func (r *Registry) RunAgent(ctx context.Context, agentID, description, agentType string, prepare func(*Task) error, run func(context.Context, *Task) (string, error)) (*Task, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	t, err := r.start(agentID, description, agentType, cancel, false, prepare)
	if err != nil {
		return nil, "", err
	}
	if t == nil {
		out, err := run(ctx, nil)
		return nil, out, err
	}
	out, err := r.execute(ctx, cancel, t, false, run)
	return t, out, err
}

func (r *Registry) start(agentID, description, agentType string, cancel context.CancelFunc, background bool, prepare func(*Task) error) (*Task, error) {
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("session is shutting down")
	}
	if background && r.running >= MaxConcurrent {
		r.mu.Unlock()
		return nil, fmt.Errorf("too many background agents running (max %d); wait for one to finish or run this agent synchronously", MaxConcurrent)
	}
	if r.launched >= MaxPerSession {
		r.mu.Unlock()
		if !background {
			return nil, nil
		}
		return nil, fmt.Errorf("background agent limit reached for this session (max %d)", MaxPerSession)
	}

	if agentID == "" {
		agentID = uuid.NewString()
	}
	if r.hasAgentIDLocked(agentID) {
		r.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("background agent identity %s already exists", agentID)
	}
	taskID := r.uniqueTaskIDLocked(agentID)
	t := &Task{
		ID:          taskID,
		AgentID:     agentID,
		Description: description,
		AgentType:   agentType,
		Started:     time.Now(),
		status:      StatusRunning,
		cancel:      cancel,
		seq:         1,
		registry:    r,
	}
	r.tasks = append(r.tasks, t)
	r.running++
	r.launched++
	r.mu.Unlock()
	ready := false
	defer func() {
		if !ready {
			r.mu.Lock()
			r.removeTaskLocked(t)
			r.running--
			r.launched--
			r.mu.Unlock()
			cancel()
		}
	}()
	if prepare != nil {
		if err := prepare(t); err != nil {
			return nil, err
		}
	}
	if err := r.persist(); err != nil {
		return nil, err
	}
	if r.ctx.Err() != nil {
		return nil, errors.New("session is shutting down")
	}

	ready = true
	return t, nil
}

// Adopt registers an already-finished run (a synchronous agent call) so it can
// be inspected and resumed like a background task. It emits no event — the
// result was already delivered inline. Returns nil when the registry is closed
// or at capacity; the caller then skips follow-up support.
func (r *Registry) Adopt(description, agentType, result string, elapsed time.Duration) *Task {
	t, _ := r.AdoptAgent(uuid.NewString(), description, agentType, result, elapsed, nil)
	return t
}

// AdoptAgent is Adopt with durable child state installed before it becomes
// visible to agent_send or UI readers.
func (r *Registry) AdoptAgent(agentID, description, agentType, result string, elapsed time.Duration, prepare func(*Task) error) (*Task, error) {
	r.mu.Lock()
	if r.ctx.Err() != nil || r.launched >= MaxPerSession {
		r.mu.Unlock()
		return nil, nil
	}

	now := time.Now()
	if agentID == "" {
		agentID = uuid.NewString()
	}
	if r.hasAgentIDLocked(agentID) {
		r.mu.Unlock()
		return nil, fmt.Errorf("background agent identity %s already exists", agentID)
	}
	taskID := r.uniqueTaskIDLocked(agentID)
	t := &Task{
		ID:          taskID,
		AgentID:     agentID,
		Description: description,
		AgentType:   agentType,
		Started:     now.Add(-elapsed),
		status:      StatusDone,
		result:      result,
		finished:    now,
		seq:         1,
		registry:    r,
	}
	r.tasks = append(r.tasks, t)
	r.launched++
	r.mu.Unlock()
	ready := false
	defer func() {
		if !ready {
			r.mu.Lock()
			r.removeTaskLocked(t)
			r.launched--
			r.mu.Unlock()
		}
	}()
	if prepare != nil {
		if err := prepare(t); err != nil {
			return nil, err
		}
	}
	if err := r.persist(); err != nil {
		return nil, err
	}
	if r.ctx.Err() != nil {
		return nil, errors.New("session is shutting down")
	}
	ready = true
	return t, nil
}

func (r *Registry) removeTaskLocked(target *Task) {
	if i := slices.Index(r.tasks, target); i >= 0 {
		r.tasks = slices.Delete(r.tasks, i, i+1)
	}
}

func (r *Registry) hasAgentIDLocked(agentID string) bool {
	for _, task := range r.tasks {
		if task.AgentID == agentID {
			return true
		}
	}
	return false
}

func (r *Registry) uniqueTaskIDLocked(agentID string) string {
	base := agentID
	if len(base) > 8 {
		base = base[:8]
	}
	candidate := base
	for suffix := 2; ; suffix++ {
		available := true
		for _, task := range r.tasks {
			if task.ID == candidate {
				available = false
				break
			}
		}
		if available {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
}

// Relaunch restarts a finished task with a new run — the agent_send follow-up
// path. The task keeps its id and history; status, timing, and cancellation
// reset for the new run, and completion fires another event.
func (r *Registry) Relaunch(t *Task, run func(ctx context.Context, t *Task) (string, error)) error {
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return fmt.Errorf("session is shutting down")
	}
	if r.running >= MaxConcurrent {
		r.mu.Unlock()
		return fmt.Errorf("too many background agents running (max %d); wait for one to finish", MaxConcurrent)
	}
	if r.launched >= MaxPerSession {
		r.mu.Unlock()
		return fmt.Errorf("background agent limit reached for this session (max %d)", MaxPerSession)
	}

	t.mu.Lock()
	if t.status == StatusRunning || t.finishing {
		t.mu.Unlock()
		r.mu.Unlock()
		return fmt.Errorf("agent %s is still running; its result arrives as a task notification", t.ID)
	}
	prevStatus := t.status
	prevStopped := t.stopped
	prevResult := t.result
	prevActivity := t.activity
	prevFinished := t.finished
	prevStarted := t.Started
	prevSeq := t.seq
	ctx, cancel := context.WithTimeout(r.ctx, MaxRunDuration)
	ctx = tool.WithBackgroundOrigin(ctx, t.AgentID)
	t.status = StatusRunning
	t.stopped = false
	t.result = ""
	t.activity = ""
	t.finished = time.Time{}
	t.cancel = cancel
	t.Started = time.Now()
	t.seq++
	t.mu.Unlock()

	r.running++
	r.launched++
	r.mu.Unlock()
	started := false
	defer func() {
		if started {
			return
		}
		t.mu.Lock()
		t.status = prevStatus
		t.stopped = prevStopped
		t.result = prevResult
		t.activity = prevActivity
		t.finished = prevFinished
		t.Started = prevStarted
		t.seq = prevSeq
		t.cancel = nil
		t.mu.Unlock()
		r.mu.Lock()
		r.running--
		r.launched--
		r.mu.Unlock()
		cancel()
	}()
	if err := r.persist(); err != nil {
		return err
	}
	if r.ctx.Err() != nil {
		return errors.New("session is shutting down")
	}

	go r.execute(ctx, cancel, t, true, run)
	started = true
	return nil
}

func (r *Registry) execute(ctx context.Context, cancel context.CancelFunc, t *Task, notify bool, run func(ctx context.Context, t *Task) (string, error)) (string, error) {
	defer cancel()
	result, err := func() (out string, runErr error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("agent panicked: %v", recovered)
			}
		}()
		return run(ctx, t)
	}()
	if err == nil {
		err = ctx.Err()
	}

	t.mu.Lock()
	t.finishing = true
	t.finished = time.Now()
	switch {
	case t.stopped:
		t.status = StatusStopped
		if err == nil {
			err = context.Canceled
		}
		if result != "" {
			result = "Agent stopped. Output below is incomplete:\n\n" + result
		} else {
			result = "Agent stopped before producing a final report."
		}
	case err != nil:
		t.status = StatusFailed
		if result != "" {
			result = fmt.Sprintf("error: %v\n\nOutput below is incomplete:\n%s", err, result)
		} else {
			result = fmt.Sprintf("error: %v", err)
		}
	default:
		t.status = StatusDone
	}
	t.result = result
	t.mu.Unlock()

	persistErr := r.persistCompletion()
	t.mu.Lock()
	if persistErr != nil && r.ctx.Err() == nil {
		t.result = fmt.Sprintf("error: could not save background task completion: %v\nExecution status: %s. The output below remains available in this session but may be lost after restart. Do not rerun the task solely because saving failed.\n\n%s", persistErr, t.status, t.result)
		t.status = StatusFailed
		err = errors.Join(err, errors.New(t.result))
	}
	t.finishing = false
	t.cancel = nil
	result = t.result
	ev := Event{
		Task:        t,
		ID:          t.ID,
		Description: t.Description,
		AgentType:   t.AgentType,
		Seq:         t.seq,
		Status:      t.status,
		Result:      t.result,
		Elapsed:     t.finished.Sub(t.Started),
	}
	t.mu.Unlock()

	r.mu.Lock()
	r.running--
	r.mu.Unlock()

	if notify {
		r.Publish(ev)
	}
	return result, err
}

func (r *Registry) Get(id string) *Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (r *Registry) List() []*Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.tasks)
}

func (r *Registry) Counts() (running, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, len(r.tasks)
}

func (r *Registry) Stop(id string) error {
	t := r.Get(id)
	if t == nil {
		return fmt.Errorf("no background agent with id %s", id)
	}
	t.mu.Lock()
	if t.status != StatusRunning {
		status := t.status
		t.mu.Unlock()
		return fmt.Errorf("agent %s already finished (%s)", id, status)
	}
	t.stopped = true
	cancel := t.cancel
	t.mu.Unlock()
	// execute persists the stopped result after the run unwinds. The stop
	// request itself has no durable transition until cancellation completes.
	if cancel != nil {
		cancel()
	}
	return nil
}

// Close cancels all running tasks and stops event delivery.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	// Exclude new writes and wait for an append or atomic snapshot already in
	// progress before returning. Canceled runs may unwind later, but they can no
	// longer recreate durable state after their session directory is deleted.
	r.persistGate.Lock()
	defer r.persistGate.Unlock()
	r.cancel()
}
