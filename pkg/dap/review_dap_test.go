package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	godap "github.com/google/go-dap"
)

// reviewDAPAdapter is a scriptable adapter: each instance owns one thread,
// one frame named after value, and one variable whose value is value.
type reviewDAPAdapter struct {
	t       *testing.T
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
	seq     int

	thread         int
	value          string
	stopOnEntry    bool
	configuring    chan struct{} // closed when configurationDone arrives
	holdConfig     chan struct{} // configurationDone waits for it
	holdDisconnect chan struct{} // disconnect waits for it
	child          map[string]any
	childStarted   chan error
	filters        []godap.ExceptionBreakpointsFilter

	mu          sync.Mutex
	launchSeq   int
	commands    []string
	breakpoints map[string][]int
	exceptions  [][]string
}

func newReviewDAPAdapter(t *testing.T, thread int, value string) *reviewDAPAdapter {
	return &reviewDAPAdapter{t: t, thread: thread, value: value, breakpoints: map[string][]int{}, childStarted: make(chan error, 1)}
}

func (adapter *reviewDAPAdapter) connect() net.Conn {
	client, server := net.Pipe()
	adapter.conn, adapter.reader = server, bufio.NewReader(server)
	go adapter.serve()
	return client
}

func (adapter *reviewDAPAdapter) received(command string, thread int) bool {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return slices.Contains(adapter.commands, fmt.Sprintf("%s:%d", command, thread))
}

func (adapter *reviewDAPAdapter) lines(path string) []int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return slices.Clone(adapter.breakpoints[filepath.Clean(path)])
}

func (adapter *reviewDAPAdapter) lastExceptions() []string {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.exceptions) == 0 {
		return nil
	}
	return adapter.exceptions[len(adapter.exceptions)-1]
}

func (adapter *reviewDAPAdapter) response(seq int, command string) godap.Response {
	return godap.Response{ProtocolMessage: godap.ProtocolMessage{Type: "response"}, RequestSeq: seq, Success: true, Command: command}
}

func (adapter *reviewDAPAdapter) event(name string) godap.Event {
	return godap.Event{ProtocolMessage: godap.ProtocolMessage{Type: "event"}, Event: name}
}

func (adapter *reviewDAPAdapter) fail(seq int, command, message string) {
	response := adapter.response(seq, command)
	response.Success, response.Message = false, message
	adapter.send(&godap.ErrorResponse{Response: response})
}

func (adapter *reviewDAPAdapter) send(message godap.Message) {
	adapter.writeMu.Lock()
	defer adapter.writeMu.Unlock()
	adapter.seq++
	switch message := message.(type) {
	case godap.ResponseMessage:
		message.GetResponse().Seq = adapter.seq
	case godap.EventMessage:
		message.GetEvent().Seq = adapter.seq
	case godap.RequestMessage:
		message.GetRequest().Seq = adapter.seq
	}
	_ = godap.WriteProtocolMessage(adapter.conn, message)
}

func (adapter *reviewDAPAdapter) stopped(reason string) {
	adapter.send(&godap.StoppedEvent{Event: adapter.event("stopped"), Body: godap.StoppedEventBody{Reason: reason, ThreadId: adapter.thread, AllThreadsStopped: true}})
}

func (adapter *reviewDAPAdapter) record(command string, thread int) {
	adapter.mu.Lock()
	adapter.commands = append(adapter.commands, fmt.Sprintf("%s:%d", command, thread))
	adapter.mu.Unlock()
}

func (adapter *reviewDAPAdapter) serve() {
	defer adapter.conn.Close()
	for {
		message, err := godap.ReadProtocolMessage(adapter.reader)
		if err != nil {
			return
		}
		switch request := message.(type) {
		case *godap.InitializeRequest:
			adapter.send(&godap.InitializeResponse{Response: adapter.response(request.Seq, "initialize"), Body: godap.Capabilities{SupportsConfigurationDoneRequest: true, ExceptionBreakpointFilters: adapter.filters}})
		case *godap.LaunchRequest:
			adapter.mu.Lock()
			adapter.launchSeq = request.Seq
			adapter.mu.Unlock()
			adapter.send(&godap.InitializedEvent{Event: adapter.event("initialized")})
		case *godap.SetBreakpointsRequest:
			var lines []int
			var verified []godap.Breakpoint
			for index, breakpoint := range request.Arguments.Breakpoints {
				lines = append(lines, breakpoint.Line)
				verified = append(verified, godap.Breakpoint{Id: request.Seq*100 + index, Verified: true, Line: breakpoint.Line})
			}
			adapter.mu.Lock()
			adapter.breakpoints[filepath.Clean(request.Arguments.Source.Path)] = lines
			adapter.mu.Unlock()
			adapter.send(&godap.SetBreakpointsResponse{Response: adapter.response(request.Seq, "setBreakpoints"), Body: godap.SetBreakpointsResponseBody{Breakpoints: verified}})
		case *godap.SetExceptionBreakpointsRequest:
			adapter.mu.Lock()
			adapter.exceptions = append(adapter.exceptions, slices.Clone(request.Arguments.Filters))
			adapter.mu.Unlock()
			adapter.send(&godap.SetExceptionBreakpointsResponse{Response: adapter.response(request.Seq, "setExceptionBreakpoints")})
		case *godap.ConfigurationDoneRequest:
			if adapter.configuring != nil {
				close(adapter.configuring)
			}
			if adapter.holdConfig != nil {
				<-adapter.holdConfig
			}
			adapter.send(&godap.ConfigurationDoneResponse{Response: adapter.response(request.Seq, "configurationDone")})
			adapter.mu.Lock()
			launchSeq := adapter.launchSeq
			adapter.mu.Unlock()
			adapter.send(&godap.LaunchResponse{Response: adapter.response(launchSeq, "launch")})
			if adapter.stopOnEntry {
				adapter.stopped("entry")
			}
			if adapter.child != nil {
				adapter.send(&godap.StartDebuggingRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Type: "request"}, Command: "startDebugging"}, Arguments: godap.StartDebuggingRequestArguments{Request: "launch", Configuration: adapter.child}})
			}
		case *godap.StartDebuggingResponse:
			adapter.childStarted <- nil
		case *godap.ErrorResponse:
			adapter.childStarted <- errors.New(request.Message)
		case *godap.ThreadsRequest:
			adapter.send(&godap.ThreadsResponse{Response: adapter.response(request.Seq, "threads"), Body: godap.ThreadsResponseBody{Threads: []godap.Thread{{Id: adapter.thread, Name: "main"}}}})
		case *godap.StackTraceRequest:
			if request.Arguments.ThreadId != adapter.thread {
				adapter.fail(request.Seq, "stackTrace", "unknown thread")
				continue
			}
			adapter.send(&godap.StackTraceResponse{Response: adapter.response(request.Seq, "stackTrace"), Body: godap.StackTraceResponseBody{
				StackFrames: []godap.StackFrame{{Id: 21, Name: adapter.value, Line: 7, Column: 1}}, TotalFrames: 1,
			}})
		case *godap.ScopesRequest:
			adapter.send(&godap.ScopesResponse{Response: adapter.response(request.Seq, "scopes"), Body: godap.ScopesResponseBody{Scopes: []godap.Scope{{Name: "Locals", VariablesReference: 31}}}})
		case *godap.VariablesRequest:
			adapter.send(&godap.VariablesResponse{Response: adapter.response(request.Seq, "variables"), Body: godap.VariablesResponseBody{Variables: []godap.Variable{{Name: "answer", Value: adapter.value}}}})
		case *godap.NextRequest:
			adapter.record("next", request.Arguments.ThreadId)
			adapter.send(&godap.NextResponse{Response: adapter.response(request.Seq, "next")})
			adapter.stopped("step")
		case *godap.ContinueRequest:
			adapter.record("continue", request.Arguments.ThreadId)
			adapter.send(&godap.ContinueResponse{Response: adapter.response(request.Seq, "continue"), Body: godap.ContinueResponseBody{AllThreadsContinued: true}})
		case *godap.DisconnectRequest:
			if adapter.holdDisconnect != nil {
				<-adapter.holdDisconnect
			}
			adapter.send(&godap.DisconnectResponse{Response: adapter.response(request.Seq, "disconnect")})
			return
		case godap.RequestMessage:
			adapter.send(&godap.Response{ProtocolMessage: godap.ProtocolMessage{Type: "response"}, RequestSeq: request.GetRequest().Seq, Success: true, Command: request.GetRequest().Command})
		}
	}
}

// reviewDAPChild starts a launched child session the way respondStartDebugging
// would, without attaching it to a parent yet.
func reviewDAPChild(t *testing.T, ctx context.Context, adapter *reviewDAPAdapter) *Session {
	t.Helper()
	plan := Plan{Adapter: AdapterDescriptor{Name: "fake", Language: "Test"}, Request: "launch", Arguments: map[string]any{"request": "launch", "name": adapter.value}}
	child := newConnectedSession(adapter.value, plan, adapter.connect())
	t.Cleanup(child.Close)
	if err := child.initializeAndLaunch(ctx, StartOptions{}); err != nil {
		t.Fatal(err)
	}
	if adapter.stopOnEntry {
		if _, ok := child.WaitForStop(ctx, 0); !ok {
			t.Fatalf("child %s did not stop on entry", adapter.value)
		}
	}
	return child
}

func reviewDAPParent() *Session {
	parent := &Session{id: "parent", plan: Plan{Request: "launch"}, state: StateRunning, stateChanged: make(chan struct{})}
	parent.alive.Store(true)
	return parent
}

// Bug: while an ended child finishes its disconnect handshake, the parent reports that child's terminated state although another child still runs; the gateway then sends terminated for a live session.
func TestReviewDAPEndedChildDoesNotTerminateRunningParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := reviewDAPParent()
	first := reviewDAPChild(t, ctx, newReviewDAPAdapter(t, 1, "worker A"))
	ending := newReviewDAPAdapter(t, 2, "worker B")
	ending.holdDisconnect = make(chan struct{})
	release := sync.OnceFunc(func() { close(ending.holdDisconnect) })
	defer release()
	second := reviewDAPChild(t, ctx, ending)
	if !parent.attachChild(first) || !parent.attachChild(second) {
		t.Fatal("parent rejected a child")
	}
	if state := parent.Status().State; state != StateRunning {
		t.Fatalf("parent state = %s before any child ended", state)
	}
	epoch := second.StateEpoch()
	ending.send(&godap.TerminatedEvent{Event: ending.event("terminated")})
	if status, ok := second.WaitForStop(ctx, epoch); !ok || status.State != StateTerminated {
		t.Fatalf("ending child = %+v", status)
	}
	if status := parent.Status(); status.State != StateRunning {
		t.Fatalf("parent state = %s while worker A still runs; the gateway reports this as terminated", status.State)
	}
}

// Bug: a thread id from a child that has ended is reinterpreted as a native thread id of another child, so stackTrace and stepping reach the wrong debuggee instead of failing.
func TestReviewDAPStaleChildThreadIsNotRoutedToAnotherChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := reviewDAPParent()
	ended := newReviewDAPAdapter(t, 7, "worker A")
	ended.stopOnEntry = true
	other := newReviewDAPAdapter(t, 1, "worker B")
	other.stopOnEntry = true
	first, second := reviewDAPChild(t, ctx, ended), reviewDAPChild(t, ctx, other)
	if !parent.attachChild(first) || !parent.attachChild(second) {
		t.Fatal("parent rejected a child")
	}
	threads, err := parent.Threads(ctx)
	if err != nil || len(threads) != 2 {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	stale := threads[slices.IndexFunc(threads, func(thread Thread) bool { return strings.HasPrefix(thread.Name, "worker A") })].ID
	// The gateway numbers threads from 1; worker B's native thread is also 1.
	if stale != other.thread {
		t.Fatalf("precondition: worker A's gateway thread = %d, want %d", stale, other.thread)
	}
	first.Close()
	if children := parent.childSessions(); len(children) != 1 || children[0] != second {
		t.Fatalf("worker A was not removed: %v", children)
	}
	if frames, _, err := parent.StackTrace(ctx, stale, 0, 20); err == nil {
		t.Errorf("stackTrace of worker A's ended thread returned %+v", frames)
	}
	if err := parent.Next(ctx, stale); err == nil || other.received("next", other.thread) {
		t.Errorf("next on worker A's ended thread stepped worker B (error %v)", err)
	}
}

// Bug: a sibling child ending invalidates frame and variable references of a child that is still suspended at the same stop.
func TestReviewDAPSiblingExitKeepsStoppedChildReferences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := reviewDAPParent()
	inspected := newReviewDAPAdapter(t, 11, "worker A")
	inspected.stopOnEntry = true
	first := reviewDAPChild(t, ctx, inspected)
	second := reviewDAPChild(t, ctx, newReviewDAPAdapter(t, 12, "worker B"))
	if !parent.attachChild(first) || !parent.attachChild(second) {
		t.Fatal("parent rejected a child")
	}
	status := parent.Status()
	if status.State != StateStopped || status.Stop == nil {
		t.Fatalf("parent status = %+v", status)
	}
	frames, _, err := parent.StackTrace(ctx, status.Stop.ThreadID, 0, 20)
	if err != nil || len(frames) != 1 || frames[0].Name != "worker A" {
		t.Fatalf("frames = %+v, %v", frames, err)
	}
	scopes, err := parent.Scopes(ctx, frames[0].ID)
	if err != nil || len(scopes) != 1 {
		t.Fatalf("scopes = %+v, %v", scopes, err)
	}
	second.Close()
	if after := parent.Status(); after.Stop == nil || after.Stop.Sequence != status.Stop.Sequence {
		t.Fatalf("worker A left its stop: %+v", after)
	}
	variables, err := parent.Variables(ctx, scopes[0].VariablesReference, 0, 0)
	if err != nil || len(variables) != 1 || variables[0].Value != "worker A" {
		t.Fatalf("variables of the unchanged stop = %+v, %v", variables, err)
	}
}

// Bug: exception filters chosen after a starting session read them but before it becomes active never reach it; source breakpoints are resynced in that window, exception filters are not.
func TestReviewDAPManagerAppliesExceptionFiltersChangedDuringStart(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/test\n")
	adapterPath := filepath.Join(root, "dlv")
	writeTestFile(t, adapterPath, "adapter\n")
	if err := os.Chmod(adapterPath, 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := newReviewDAPAdapter(t, 1, "main")
	adapter.filters = []godap.ExceptionBreakpointsFilter{{Filter: "raised", Label: "Raised"}, {Filter: "uncaught", Label: "Uncaught", Default: true}}
	configured, finish := make(chan struct{}), make(chan struct{})
	manager := newManager(root, []AdapterDescriptor{{
		Name: "delve", Language: "Go", Command: "dlv", Markers: []string{"go.mod"}, SourceExtensions: []string{".go"},
	}}, func(string) string { return adapterPath }, func(ctx context.Context, id string, plan Plan, options StartOptions) (*Session, error) {
		session := newConnectedSession(id, plan, adapter.connect())
		session.hooks = options.hooks
		err := session.initializeAndLaunch(ctx, options)
		close(configured)
		<-finish
		return session, err
	})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() {
		_, err := manager.Start(ctx, StartOptions{Adapter: "delve"})
		started <- err
	}()
	<-configured
	if filters := adapter.lastExceptions(); !slices.Equal(filters, []string{"uncaught"}) {
		t.Fatalf("configuration sent filters %v, want the adapter default", filters)
	}
	if err := manager.SetExceptionBreakpoints(ctx, []string{"raised"}); err != nil {
		t.Fatal(err)
	}
	close(finish)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if filters := adapter.lastExceptions(); !slices.Equal(filters, []string{"raised"}) {
		t.Fatalf("adapter exception filters = %v, want the selection made before activation", filters)
	}
}

type reviewDAPConnector struct {
	mu       sync.Mutex
	adapters []*reviewDAPAdapter
}

func (connector *reviewDAPConnector) ConnectAdapter(context.Context, Plan) (io.ReadWriteCloser, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	if len(connector.adapters) == 0 {
		return nil, errors.New("no adapter left")
	}
	adapter := connector.adapters[0]
	connector.adapters = connector.adapters[1:]
	return adapter.connect(), nil
}

// Bug: a breakpoint edited while a startDebugging child configures never reaches that child; the manager resyncs late edits for root sessions but not for children.
func TestReviewDAPBreakpointsChangedDuringChildStartReachChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source := filepath.Join(t.TempDir(), "main.js")
	root := newReviewDAPAdapter(t, 1, "root")
	root.child = map[string]any{"name": "worker"}
	child := newReviewDAPAdapter(t, 2, "worker")
	child.configuring, child.holdConfig = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(child.holdConfig) })
	defer release()
	connector := &reviewDAPConnector{adapters: []*reviewDAPAdapter{root, child}}
	plan := Plan{Adapter: AdapterDescriptor{Name: "fake", Language: "Test", Transport: TransportConnect}, Request: "launch", Arguments: map[string]any{"request": "launch"}}
	parent, err := startSession(ctx, "parent", plan, StartOptions{Breakpoints: map[string][]SourceBreakpoint{source: {{Line: 5}}}, adapterConnector: connector})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	select {
	case <-child.configuring:
	case <-ctx.Done():
		t.Fatal("child never configured")
	}
	if !slices.Equal(child.lines(source), []int{5}) {
		t.Fatalf("child configured with %v", child.lines(source))
	}
	if _, err := parent.SetBreakpoints(ctx, source, []SourceBreakpoint{{Line: 5}, {Line: 9}}); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-root.childStarted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("child never started")
	}
	if lines := child.lines(source); !slices.Equal(lines, []int{5, 9}) {
		t.Fatalf("child breakpoints = %v, want the edit made while it configured", lines)
	}
}

func TestReviewDAPExceptionFiltersChangedDuringChildStartReachChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	filters, version := []string{"uncaught"}, uint64(1)
	hooks := sessionHooks{exceptionFilters: func() ([]string, bool, uint64) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(filters), true, version
	}}
	root := newReviewDAPAdapter(t, 1, "root")
	root.child = map[string]any{"name": "worker"}
	child := newReviewDAPAdapter(t, 2, "worker")
	child.filters = []godap.ExceptionBreakpointsFilter{{Filter: "raised", Label: "Raised"}, {Filter: "uncaught", Label: "Uncaught"}}
	child.configuring, child.holdConfig = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(child.holdConfig) })
	defer release()
	connector := &reviewDAPConnector{adapters: []*reviewDAPAdapter{root, child}}
	plan := Plan{Adapter: AdapterDescriptor{Name: "fake", Language: "Test", Transport: TransportConnect}, Request: "launch", Arguments: map[string]any{"request": "launch"}}
	parent, err := startSession(ctx, "parent", plan, StartOptions{adapterConnector: connector, hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	<-child.configuring
	if got := child.lastExceptions(); !slices.Equal(got, []string{"uncaught"}) {
		t.Fatalf("child configured with %v", got)
	}
	mu.Lock()
	filters, version = []string{"raised"}, 2
	mu.Unlock()
	if err := parent.SetExceptionBreakpoints(ctx, []string{"raised"}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-root.childStarted; err != nil {
		t.Fatal(err)
	}
	if got := child.lastExceptions(); !slices.Equal(got, []string{"raised"}) {
		t.Fatalf("child exception filters = %v, want the selection made while it configured", got)
	}
}

// The parent's reported version must grow whenever what it presents changes,
// including switches between a child's state and its own.
func TestReviewDAPParentStatusVersionIsMonotonic(t *testing.T) {
	parent := reviewDAPParent()
	first := &Session{id: "first", state: StateRunning, stateChanged: make(chan struct{})}
	if !parent.attachChild(first) {
		t.Fatal("parent rejected a child")
	}
	versions := []uint64{parent.Status().StateVersion}
	if again := parent.Status().StateVersion; again != versions[0] {
		t.Fatalf("unchanged status changed version %d -> %d", versions[0], again)
	}
	first.setState(StateTerminated, nil)
	status := parent.Status()
	if status.State != StateRunning || status.SessionID != "parent" {
		t.Fatalf("status with only an ended child = %+v", status)
	}
	versions = append(versions, status.StateVersion)
	second := &Session{id: "second", state: StateRunning, stateChanged: make(chan struct{})}
	if !parent.attachChild(second) {
		t.Fatal("parent rejected a child")
	}
	versions = append(versions, parent.Status().StateVersion)
	second.mu.Lock()
	second.recordStopLocked(Stop{Reason: "breakpoint", ThreadID: 4})
	second.notifyStateLocked()
	second.mu.Unlock()
	status = parent.Status()
	if status.State != StateStopped || status.Stop == nil || status.Stop.ThreadID <= 0 {
		t.Fatalf("stopped child status = %+v", status)
	}
	versions = append(versions, status.StateVersion)
	for index := 1; index < len(versions); index++ {
		if versions[index] <= versions[index-1] {
			t.Fatalf("versions %v are not increasing", versions)
		}
	}
	if epoch := parent.StateEpoch(); epoch < versions[len(versions)-1] {
		t.Fatalf("StateEpoch %d is behind the reported version %d", epoch, versions[len(versions)-1])
	}
}

func TestReviewDAPOutputCarryKeepsStreamsApart(t *testing.T) {
	var buffer outputBuffer
	smile := "😀"
	buffer.append("stdout", "a"+smile[:1])
	buffer.append("stderr", "\xffwarn\n")
	buffer.append("stdout", smile[1:3])
	buffer.append("stdout", smile[3:]+"\n")
	chunks, end := buffer.since(0)
	if len(chunks) != 3 || chunks[0].Output != "a" || chunks[1].Output != "\xffwarn\n" || chunks[2].Output != smile+"\n" {
		t.Fatalf("chunks = %+v", chunks)
	}
	text, _ := buffer.snapshot()
	if want := reviewDAPBrowserLength(t, text); end != want {
		t.Fatalf("offset %d, browser length %d", end, want)
	}
	// A sequence that can never complete is not held back.
	buffer.append("console", "x\xe2\x82y")
	if _, after := buffer.since(end); after != end+4 {
		t.Fatalf("invalid bytes were held: end %d -> %d", end, after)
	}
}

// Bug: Status of a session with children reports the focused child's request and terminal, so the gateway's disconnect default and the debuggee terminal follow the child rather than the session the browser launched.
func TestReviewDAPParentStatusKeepsOwnRequestAndTerminal(t *testing.T) {
	parent := reviewDAPParent()
	parent.terminalID = "debuggee-terminal"
	child := &Session{id: "child", plan: Plan{Request: "attach"}, state: StateRunning, stateChanged: make(chan struct{})}
	if !parent.attachChild(child) {
		t.Fatal("parent rejected a child")
	}
	status := parent.Status()
	if status.Request != "launch" || status.TerminalID != "debuggee-terminal" {
		t.Fatalf("status request = %q terminal = %q, want the launched session's", status.Request, status.TerminalID)
	}
}

func reviewDAPBrowserLength(t *testing.T, text string) int64 {
	t.Helper()
	wire, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	var decoded string
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	return int64(len(utf16.Encode([]rune(decoded))))
}

// Bug: output read in byte chunks can split a UTF-8 sequence; each half is counted as replacement characters, so offsets exceed the UTF-16 length of the joined text the browser receives.
func TestReviewDAPOutputOffsetsMatchSplitRune(t *testing.T) {
	var buffer outputBuffer
	euro := "€"
	buffer.append("stdout", "price: "+euro[:2])
	buffer.append("stdout", euro[2:]+"\n")
	text, end := buffer.snapshot()
	if want := reviewDAPBrowserLength(t, text); end != want {
		t.Fatalf("offset after %q = %d, browser length = %d", text, end, want)
	}
}

func TestReviewDAPOutputBufferStress(t *testing.T) {
	var buffer outputBuffer
	var wait sync.WaitGroup
	for writer := range 4 {
		wait.Go(func() {
			for index := range 2000 {
				buffer.append([]string{"stdout", "stderr"}[writer%2], fmt.Sprintf("w%d-%04d 😀é\n", writer, index))
			}
		})
	}
	done := make(chan struct{})
	readers := sync.WaitGroup{}
	for range 2 {
		readers.Go(func() {
			offset := int64(0)
			for {
				select {
				case <-done:
					return
				default:
				}
				chunks, end := buffer.since(offset)
				for index, chunk := range chunks {
					next := end
					if index+1 < len(chunks) {
						next = chunks[index+1].Offset
					}
					if chunk.Offset < offset || chunk.Offset+utf16Length(chunk.Output) != next {
						t.Errorf("chunk %+v does not end at %d (reader offset %d)", chunk, next, offset)
						return
					}
				}
				offset = end
			}
		})
	}
	wait.Wait()
	close(done)
	readers.Wait()
	text, end := buffer.snapshot()
	if !strings.HasPrefix(text, "[earlier debugger output truncated]\n") || len(text) > maxOutputBytes+64 {
		t.Fatalf("retained %d bytes", len(text))
	}
	chunks, since := buffer.since(0)
	retained := int64(0)
	for _, chunk := range chunks {
		retained += utf16Length(chunk.Output)
	}
	if since != end || chunks[0].Offset+retained != end {
		t.Fatalf("retained window [%d, %d) does not end at %d", chunks[0].Offset, chunks[0].Offset+retained, end)
	}
}

func TestReviewDAPReviewedPlansExpireAndEvict(t *testing.T) {
	manager := newManager(t.TempDir(), nil, func(string) string { return "" }, nil)
	first := manager.RememberPlan("first")
	time.Sleep(time.Millisecond)
	ids := []string{first}
	for index := range reviewedPlanLimit {
		ids = append(ids, manager.RememberPlan(index))
		time.Sleep(time.Millisecond)
	}
	if _, ok := manager.ReviewedPlan(first); ok {
		t.Fatal("the oldest plan survived the plan limit")
	}
	for _, id := range ids[1:] {
		if _, ok := manager.ReviewedPlan(id); !ok {
			t.Fatalf("plan %s was evicted early", id)
		}
	}
	if len(manager.plans) != reviewedPlanLimit {
		t.Fatalf("retained %d plans", len(manager.plans))
	}
	manager.mu.Lock()
	expired := manager.plans[ids[1]]
	expired.created = time.Now().Add(-reviewedPlanTTL - time.Second)
	manager.plans[ids[1]] = expired
	manager.mu.Unlock()
	if _, ok := manager.ReviewedPlan(ids[1]); ok {
		t.Fatal("an expired plan was returned")
	}
	if _, ok := manager.ReviewedPlan(""); ok {
		t.Fatal("an empty plan id was accepted")
	}
	// Remembering prunes expired plans before evicting live ones.
	manager.RememberPlan("next")
	if _, ok := manager.ReviewedPlan(ids[2]); !ok {
		t.Fatal("a live plan was evicted while an expired one was retained")
	}
}

func TestReviewDAPConcurrentRememberPlan(t *testing.T) {
	manager := newManager(t.TempDir(), nil, func(string) string { return "" }, nil)
	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Go(func() {
			for index := range 200 {
				id := manager.RememberPlan(worker*1000 + index)
				_, _ = manager.ReviewedPlan(id)
			}
		})
	}
	wait.Wait()
	if len(manager.plans) > reviewedPlanLimit {
		t.Fatalf("retained %d plans, limit %d", len(manager.plans), reviewedPlanLimit)
	}
}

// Parallel gutter edits, status reads and verification must keep ids stable
// per position and never lose the last edit of a file.
func TestReviewDAPManagerBreakpointStress(t *testing.T) {
	manager := newManager(t.TempDir(), nil, func(string) string { return "" }, nil)
	defer manager.Close()
	ctx := context.Background()
	paths := []string{"/workspace/a.go", "/workspace/b.go", "/workspace/../workspace/c.go"}
	var wait sync.WaitGroup
	for worker := range 6 {
		wait.Go(func() {
			path := paths[worker%len(paths)]
			for index := range 300 {
				values := []SourceBreakpoint{{Line: 1 + index%5}, {Line: 10}}
				resolved, err := manager.SetBreakpoints(ctx, path, values)
				if err != nil || len(resolved) != 2 || resolved[0].ID == 0 || resolved[1].ID == 0 {
					t.Errorf("resolved = %+v, %v", resolved, err)
					return
				}
				_ = manager.ResolvedBreakpoints()
				_ = manager.ResolvedSourceBreakpoints(path)
				manager.resolveBreakpoint(path, 10, 0, Breakpoint{Verified: true, Line: 10})
			}
		})
	}
	wait.Wait()
	for _, path := range paths {
		ids := map[int]bool{}
		for _, breakpoint := range manager.ResolvedSourceBreakpoints(path) {
			if breakpoint.ID == 0 || ids[breakpoint.ID] {
				t.Fatalf("%s: duplicate or missing id in %+v", path, manager.ResolvedSourceBreakpoints(path))
			}
			ids[breakpoint.ID] = true
		}
		if len(ids) != 2 {
			t.Fatalf("%s: %d breakpoints, want 2", path, len(ids))
		}
	}
	// Removing every breakpoint drops their verification.
	for _, path := range paths {
		if _, err := manager.SetBreakpoints(ctx, path, nil); err != nil {
			t.Fatal(err)
		}
	}
	if resolved := manager.ResolvedBreakpoints(); len(resolved) != 0 {
		t.Fatalf("removed breakpoints kept verification: %+v", resolved)
	}
}
