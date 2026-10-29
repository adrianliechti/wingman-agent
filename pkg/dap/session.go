package dap

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	godap "github.com/google/go-dap"
)

const (
	maxOutputBytes         = 64 * 1024
	disconnectTimeout      = 3 * time.Second
	adapterExitGracePeriod = time.Second
)

var errSessionClosed = errors.New("debug session closed")

type responseResult struct {
	message godap.ResponseMessage
	err     error
}

type pendingRequest struct {
	seq      int
	command  string
	response <-chan responseResult
}

type outputBuffer struct {
	mu      sync.Mutex
	chunks  []OutputChunk
	size    int
	end     int64
	trimmed bool
	changed chan struct{}
}

type terminalBinding struct {
	process TerminalProcess
	cancel  func()
}

var stopSequence atomic.Uint64

func (buffer *outputBuffer) append(category, value string) {
	// Telemetry is for the client's telemetry pipeline, never the user.
	if value == "" || category == "telemetry" {
		return
	}
	switch category {
	case "stdout", "stderr", "console", "important":
	default:
		category = "console"
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.changed != nil {
		close(buffer.changed)
		buffer.changed = nil
	}
	buffer.chunks = append(buffer.chunks, OutputChunk{Category: category, Output: value, Offset: buffer.end})
	buffer.end += utf16Length(value)
	buffer.size += len(value)
	for buffer.size > maxOutputBytes && len(buffer.chunks) > 0 {
		buffer.trimmed = true
		first := &buffer.chunks[0]
		if excess := buffer.size - maxOutputBytes; excess < len(first.Output) {
			for excess < len(first.Output) && !utf8.RuneStart(first.Output[excess]) {
				excess++
			}
			first.Offset += utf16Length(first.Output[:excess])
			first.Output = first.Output[excess:]
			buffer.size -= excess
			if first.Output != "" {
				break
			}
		} else {
			buffer.size -= len(first.Output)
		}
		buffer.chunks = buffer.chunks[1:]
	}
	if len(buffer.chunks) > 64 && len(buffer.chunks)*2 < cap(buffer.chunks) {
		buffer.chunks = slices.Clone(buffer.chunks)
	}
}

func (buffer *outputBuffer) String() string {
	text, _ := buffer.snapshot()
	return text
}

// snapshot returns the retained text and the offset that follows it.
func (buffer *outputBuffer) snapshot() (string, int64) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	var text strings.Builder
	if buffer.trimmed {
		text.WriteString("[earlier debugger output truncated]\n")
	}
	for _, chunk := range buffer.chunks {
		text.WriteString(chunk.Output)
	}
	return text.String(), buffer.end
}

// since returns chunks starting at or after offset. An offset that predates
// the retained window returns everything still retained.
func (buffer *outputBuffer) since(offset int64) ([]OutputChunk, int64) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	index, _ := slices.BinarySearchFunc(buffer.chunks, offset, func(chunk OutputChunk, offset int64) int {
		return cmp.Compare(chunk.Offset, offset)
	})
	return slices.Clone(buffer.chunks[index:]), buffer.end
}

func (buffer *outputBuffer) changes() <-chan struct{} {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.changed == nil {
		buffer.changed = make(chan struct{})
	}
	return buffer.changed
}

func utf16Length(value string) int64 {
	var length int64
	for _, r := range value {
		length++
		if r >= 0x10000 {
			length++
		}
	}
	return length
}

type Session struct {
	id         string
	plan       Plan
	connection *adapterConnection
	startedAt  time.Time
	output     *outputBuffer
	hooks      sessionHooks

	writeMu sync.Mutex
	seq     int

	pendingMu sync.Mutex
	pending   map[int]chan responseResult

	mu           sync.Mutex
	state        State
	stop         *Stop
	threadStops  map[int]Stop
	allStopped   bool
	exitCode     *int
	terminalErr  error
	capabilities Capabilities
	adapter      godap.Capabilities
	terminalID   string
	terminals    []terminalBinding
	parent       *Session
	child        *Session // the focused child; inspection and control go here
	children     []*Session
	breakpoints  map[string][]SourceBreakpoint
	adapterIDs   map[int]breakpointRef
	configured   uint64 // editor breakpoint version applied during configuration
	functions    []FunctionBreakpoint
	preLaunch    *debugProcess
	stateVersion uint64
	stateChanged chan struct{}
	initialized  chan struct{}
	initOnce     sync.Once
	launchDone   chan struct{}
	launchOnce   sync.Once
	resourceDone chan struct{}

	alive         atomic.Bool
	disconnecting atomic.Bool
	finishOnce    sync.Once
	closeOnce     sync.Once
	terminateOnce sync.Once
	disconnectMu  sync.Mutex

	terminalLauncher TerminalLauncher
}

type breakpointRef struct {
	path         string
	line, column int
}

func startSession(ctx context.Context, id string, plan Plan, options StartOptions) (*Session, error) {
	output := options.hooks.output
	if output == nil {
		output = &outputBuffer{}
	}
	session := &Session{
		id:               id,
		plan:             plan,
		startedAt:        time.Now(),
		output:           output,
		hooks:            options.hooks,
		pending:          make(map[int]chan responseResult),
		state:            StateStarting,
		stateChanged:     make(chan struct{}),
		initialized:      make(chan struct{}),
		launchDone:       make(chan struct{}),
		resourceDone:     make(chan struct{}),
		terminalLauncher: options.terminalLauncher,
		breakpoints:      cloneSourceBreakpointSets(options.Breakpoints),
		adapterIDs:       make(map[int]breakpointRef),
		functions:        slices.Clone(options.FunctionBreakpoints),
	}
	if plan.PreLaunch != nil {
		session.output.append("console", fmt.Sprintf("Starting %s: %s %s\n", plan.PreLaunch.Title, plan.PreLaunch.Command, strings.Join(plan.PreLaunch.Args, " ")))
		process, err := startDebugProcess(plan, session.output.append)
		session.preLaunch = process
		if err == nil {
			if plan.PreLaunch.WaitForExit {
				err = process.waitExit(ctx, plan.PreLaunch.Title)
			} else {
				err = process.waitReady(ctx, plan.PreLaunch.ReadyURL)
			}
		}
		if err != nil {
			failure := friendlyStartError(plan, err, session.Output())
			session.recordLaunchFailure(failure)
			return session, failure
		}
		switch {
		case plan.PreLaunch.WaitForExit:
			session.output.append("console", fmt.Sprintf("%s finished\n", plan.PreLaunch.Title))
		case plan.PreLaunch.ReadyURL != "":
			session.output.append("console", fmt.Sprintf("%s is ready at %s\n", plan.PreLaunch.Title, plan.PreLaunch.ReadyURL))
		}
	}
	connection, err := startAdapter(ctx, plan, session.output.append, options.terminalLauncher, options.adapterConnector)
	if err != nil {
		failure := friendlyStartError(plan, err, session.Output())
		session.recordLaunchFailure(failure)
		return session, failure
	}
	session.connection = connection
	session.terminalID = connection.terminalID
	session.alive.Store(true)
	go session.readLoop()
	go session.watchProcess()
	go session.watchPreLaunch()

	launchCtx, cancelLaunch := context.WithTimeout(ctx, adapterStartupTimeout)
	err = session.initializeAndLaunch(launchCtx, options)
	cancelLaunch()
	if err != nil {
		failure := friendlyStartError(plan, err, session.Output())
		session.recordLaunchFailure(failure)
		return session, failure
	}
	return session, nil
}

func (session *Session) watchPreLaunch() {
	process := session.preLaunch
	if process == nil || (session.plan.PreLaunch != nil && session.plan.PreLaunch.WaitForExit) {
		return
	}
	<-process.done
	if process.stopping.Load() || session.Status().State == StateTerminated {
		return
	}
	session.closeResourcesWithError(fmt.Errorf("Development server exited unexpectedly: %w", process.waitError()))
}

func friendlyStartError(plan Plan, err error, output string) error {
	detail := strings.ToLower(err.Error() + "\n" + output)
	switch {
	case strings.Contains(detail, "unable to find an installation of the browser"),
		strings.Contains(detail, "browser executable") && strings.Contains(detail, "not found"):
		return errors.New("No supported Chromium browser was found. Install Chrome, Chromium, Edge, or Brave, or set CHROME_PATH and try again.")
	case strings.Contains(detail, "eaddrinuse"), strings.Contains(detail, "address already in use"):
		return errors.New("The development server could not start because its port is already in use. Stop the other process or choose another port and try again.")
	case strings.Contains(detail, "econnrefused"), strings.Contains(detail, "failed to connect to target"):
		return errors.New("The debugger could not reach the development server. Check that the package script starts successfully and that its port matches the debug URL.")
	case strings.Contains(detail, "missing script"), strings.Contains(detail, "command not found"), strings.Contains(detail, "is not recognized as an internal or external command"):
		return errors.New("The package script could not start. Check the script name, install the project dependencies, and try again.")
	case strings.Contains(detail, "build failed:"):
		return errors.New("The project build failed. Fix the build errors shown in the debug output and try again.")
	default:
		return fmt.Errorf("Could not start the %s debugger: %w", plan.Adapter.Language, err)
	}
}

func (session *Session) recordLaunchFailure(err error) {
	session.mu.Lock()
	session.state = StateTerminated
	session.stop = nil
	session.terminalErr = err
	session.notifyStateLocked()
	session.mu.Unlock()
	session.closeResourcesWithError(err)
}

// newConnectedSession is used by protocol tests and by future adapter hosts
// that provide an already-connected DAP stream.
func newConnectedSession(id string, plan Plan, connection io.ReadWriteCloser) *Session {
	session := &Session{
		id:           id,
		plan:         plan,
		connection:   &adapterConnection{ReadWriteCloser: connection},
		startedAt:    time.Now(),
		output:       &outputBuffer{},
		pending:      make(map[int]chan responseResult),
		state:        StateStarting,
		stateChanged: make(chan struct{}),
		initialized:  make(chan struct{}),
		launchDone:   make(chan struct{}),
		resourceDone: make(chan struct{}),
		breakpoints:  make(map[string][]SourceBreakpoint),
		adapterIDs:   make(map[int]breakpointRef),
	}
	session.alive.Store(true)
	go session.readLoop()
	return session
}

func (session *Session) ID() string { return session.id }

func (session *Session) Status() Status {
	if child := session.childSession(); child != nil {
		status := child.Status()
		session.mu.Lock()
		status.SessionID = session.id
		status.StateVersion = session.stateVersion
		status.StartedAt = session.startedAt
		session.mu.Unlock()
		return status
	}
	return session.ownStatus()
}

func (session *Session) ownStatus() Status {
	session.mu.Lock()
	defer session.mu.Unlock()
	status := Status{
		SessionID:    session.id,
		Adapter:      session.plan.Adapter.Name,
		Language:     session.plan.Adapter.Language,
		Target:       session.plan.Target,
		Mode:         session.plan.Mode,
		Request:      session.plan.Request,
		IO:           session.plan.IO,
		TerminalID:   session.terminalID,
		Capabilities: session.capabilities,
		StateVersion: session.stateVersion,
		State:        session.state,
		ExitCode:     cloneInt(session.exitCode),
		StartedAt:    session.startedAt,
	}
	if session.stop != nil {
		stop := *session.stop
		stop.HitBreakpointIDs = slices.Clone(session.stop.HitBreakpointIDs)
		status.Stop = &stop
	}
	status.Capabilities.ExceptionFilters = slices.Clone(session.capabilities.ExceptionFilters)
	if session.state == StateStopped && !session.allStopped && len(session.threadStops) > 0 {
		status.StoppedThreads = slices.Sorted(maps.Keys(session.threadStops))
	}
	if session.terminalErr != nil &&
		!errors.Is(session.terminalErr, io.EOF) &&
		!errors.Is(session.terminalErr, errSessionClosed) {
		status.Error = session.terminalErr.Error()
	}
	return status
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// Output includes every child session; children share their parent's buffer.
func (session *Session) Output() string { return session.output.String() }

// OutputSnapshot returns the retained output and the offset that follows it.
func (session *Session) OutputSnapshot() (string, int64) { return session.output.snapshot() }

// OutputSince returns output chunks from offset and the new end offset.
func (session *Session) OutputSince(offset int64) ([]OutputChunk, int64) {
	return session.output.since(offset)
}

// Changes returns edge-triggered wakeups. Subscribe before reading Status and
// Output, then resubscribe after either channel closes. No adapter messages are
// consumed by observers, and browser disconnection never owns this process.
func (session *Session) Changes() (state, output <-chan struct{}) {
	session.mu.Lock()
	state = session.stateChanged
	session.mu.Unlock()
	return state, session.output.changes()
}

func (session *Session) initializeAndLaunch(ctx context.Context, options StartOptions) error {
	defer session.launchOnce.Do(func() { close(session.launchDone) })
	response, err := session.request(ctx, &godap.InitializeRequest{
		Request: request("initialize"),
		Arguments: godap.InitializeRequestArguments{
			ClientID:                      "wingman",
			ClientName:                    "Wingman",
			AdapterID:                     session.plan.Adapter.AdapterID,
			Locale:                        "en-US",
			LinesStartAt1:                 true,
			ColumnsStartAt1:               true,
			PathFormat:                    "path",
			SupportsVariableType:          true,
			SupportsVariablePaging:        true,
			SupportsRunInTerminalRequest:  session.plan.IO == IOTerminal && session.terminalLauncher != nil,
			SupportsStartDebuggingRequest: session.connection.childConnector() != nil,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	initialized, ok := response.(*godap.InitializeResponse)
	if !ok {
		return fmt.Errorf("initialize returned %T", response)
	}
	session.mu.Lock()
	session.adapter = initialized.Body
	session.capabilities = adapterCapabilities(initialized.Body)
	session.mu.Unlock()
	session.setState(StateConfiguring, nil)

	arguments, err := json.Marshal(session.plan.Arguments)
	if err != nil {
		return fmt.Errorf("encode launch arguments: %w", err)
	}
	var startMessage godap.RequestMessage
	switch session.plan.Request {
	case "attach":
		startMessage = &godap.AttachRequest{
			Request:   request("attach"),
			Arguments: arguments,
		}
	default:
		startMessage = &godap.LaunchRequest{
			Request:   request("launch"),
			Arguments: arguments,
		}
	}
	startPending, err := session.sendRequest(startMessage)
	if err != nil {
		return err
	}
	startResult := make(chan error, 1)
	go func() {
		_, requestErr := session.awaitRequest(ctx, startPending)
		startResult <- requestErr
	}()

	startResponseReceived := false
	startResponse := (<-chan error)(startResult)
	for {
		select {
		case <-session.initialized:
			goto configure
		case requestErr := <-startResponse:
			if requestErr != nil {
				return requestErr
			}
			// Some conforming adapters acknowledge launch before emitting the
			// initialized event. Keep waiting for the configuration barrier.
			startResponseReceived = true
			startResponse = nil
		case <-session.resourceDone:
			status := session.Status()
			if status.Error != "" {
				return errors.New(status.Error)
			}
			return errSessionClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}

configure:

	breakpointSets := cloneSourceBreakpointSets(options.Breakpoints)
	if source := session.hooks.breakpointSource; source != nil {
		// Breakpoints toggled during a prelaunch build or adapter start apply now.
		var version uint64
		breakpointSets, version = source()
		session.mu.Lock()
		session.configured = version
		session.breakpoints = cloneSourceBreakpointSets(breakpointSets)
		session.mu.Unlock()
	}
	for _, path := range slices.Sorted(maps.Keys(breakpointSets)) {
		breakpoints := breakpointSets[path]
		resolved, err := session.SetBreakpoints(ctx, path, breakpoints)
		if err != nil {
			return err
		}
		session.reportBreakpoints(path, breakpoints, resolved)
	}
	if filters, ok := session.initialExceptionFilters(); ok {
		if err := session.sendExceptionBreakpoints(ctx, filters); err != nil {
			return err
		}
	}
	if len(options.FunctionBreakpoints) > 0 {
		if !initialized.Body.SupportsFunctionBreakpoints {
			return fmt.Errorf("adapter %s does not support function breakpoints", session.plan.Adapter.Name)
		}
		if _, err := session.SetFunctionBreakpoints(ctx, options.FunctionBreakpoints); err != nil {
			return err
		}
	}
	if initialized.Body.SupportsConfigurationDoneRequest {
		if _, err := session.request(ctx, &godap.ConfigurationDoneRequest{Request: request("configurationDone")}); err != nil {
			return fmt.Errorf("configurationDone: %w", err)
		}
	}

	if !startResponseReceived {
		select {
		case requestErr := <-startResult:
			if requestErr != nil {
				return requestErr
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	session.finishConfiguration()
	return nil
}

// finishConfiguration must not overwrite a stopped or terminated event that
// arrives immediately after the final launch/configuration response.
func (session *Session) finishConfiguration() {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != StateConfiguring {
		return
	}
	session.state = StateRunning
	session.stop = nil
	session.notifyStateLocked()
}

func request(command string) godap.Request {
	return godap.Request{ProtocolMessage: godap.ProtocolMessage{Type: "request"}, Command: command}
}

func (session *Session) request(ctx context.Context, message godap.RequestMessage) (godap.ResponseMessage, error) {
	pending, err := session.sendRequest(message)
	if err != nil {
		return nil, err
	}
	return session.awaitRequest(ctx, pending)
}

func (session *Session) sendRequest(message godap.RequestMessage) (pendingRequest, error) {
	if !session.alive.Load() {
		status := session.Status()
		if status.Error != "" {
			return pendingRequest{}, fmt.Errorf("debug session is closed: %s", status.Error)
		}
		return pendingRequest{}, errors.New("debug session is closed")
	}

	session.writeMu.Lock()
	session.seq++
	seq := session.seq
	message.GetRequest().ProtocolMessage.Seq = seq
	message.GetRequest().ProtocolMessage.Type = "request"
	wait := make(chan responseResult, 1)
	session.pendingMu.Lock()
	session.pending[seq] = wait
	session.pendingMu.Unlock()
	err := godap.WriteProtocolMessage(session.connection, message)
	session.writeMu.Unlock()
	if err != nil {
		session.removePending(seq)
		return pendingRequest{}, fmt.Errorf("send %s: %w", message.GetRequest().Command, err)
	}
	return pendingRequest{seq: seq, command: message.GetRequest().Command, response: wait}, nil
}

func (session *Session) awaitRequest(ctx context.Context, pending pendingRequest) (godap.ResponseMessage, error) {
	select {
	case result := <-pending.response:
		if result.err != nil {
			return nil, result.err
		}
		if result.message == nil {
			return nil, fmt.Errorf("%s returned an empty response", pending.command)
		}
		if response := result.message.GetResponse(); !response.Success {
			return nil, dapResponseError(result.message)
		}
		return result.message, nil
	case <-ctx.Done():
		session.removePending(pending.seq)
		session.cancelAdapterRequest(pending)
		return nil, ctx.Err()
	}
}

// cancelAdapterRequest lets the adapter stop work nobody is waiting for. Its
// response to the cancelled request is ignored once the pending entry is gone.
func (session *Session) cancelAdapterRequest(pending pendingRequest) {
	session.mu.Lock()
	supported := session.adapter.SupportsCancelRequest
	session.mu.Unlock()
	if !supported || pending.command == "cancel" || pending.command == "disconnect" {
		return
	}
	cancel, err := session.sendRequest(&godap.CancelRequest{Request: request("cancel"), Arguments: &godap.CancelArguments{RequestId: pending.seq}})
	if err == nil {
		session.removePending(cancel.seq)
	}
}

func (session *Session) removePending(seq int) {
	session.pendingMu.Lock()
	delete(session.pending, seq)
	session.pendingMu.Unlock()
}

func dapResponseError(message godap.ResponseMessage) error {
	response := message.GetResponse()
	detail := response.Message
	if wire, ok := message.(*godap.ErrorResponse); ok && wire.Body.Error != nil {
		detail = wire.Body.Error.Format
		for key, value := range wire.Body.Error.Variables {
			detail = strings.ReplaceAll(detail, "{"+key+"}", value)
		}
	}
	if detail == "" {
		detail = "request failed"
	}
	return fmt.Errorf("%s: %s", response.Command, detail)
}

func (session *Session) readLoop() {
	reader := bufio.NewReader(session.connection)
	for {
		message, err := readDAPMessage(reader)
		if err != nil {
			if errors.Is(err, io.EOF) && session.Status().State != StateTerminated {
				err = errors.New("debug adapter connection closed unexpectedly")
			}
			session.closeResourcesWithError(err)
			return
		}
		switch message := message.(type) {
		case godap.ResponseMessage:
			session.pendingMu.Lock()
			wait := session.pending[message.GetResponse().RequestSeq]
			delete(session.pending, message.GetResponse().RequestSeq)
			session.pendingMu.Unlock()
			if wait != nil {
				wait <- responseResult{message: message}
			}
		case godap.EventMessage:
			session.handleEvent(message)
		case godap.RequestMessage:
			go session.handleAdapterRequest(message)
		}
	}
}

// continueResponse keeps DAP's default: an omitted allThreadsContinued means
// true, which go-dap's plain bool cannot represent.
type continueResponse struct {
	*godap.ContinueResponse
	allThreadsContinued bool
}

// readDAPMessage preserves extension compatibility. google/go-dap provides
// concrete types for the standard protocol, but adapters are explicitly
// allowed to add events and requests. Decode unknown extensions to their base
// envelope so callers can ignore custom events or reject custom requests
// without terminating an otherwise healthy session.
func readDAPMessage(reader *bufio.Reader) (godap.Message, error) {
	content, err := godap.ReadBaseMessage(reader)
	if err != nil {
		return nil, err
	}
	message, err := godap.DecodeProtocolMessage(content)
	if response, ok := message.(*godap.ContinueResponse); ok && err == nil {
		var wire struct {
			Body struct {
				AllThreadsContinued *bool `json:"allThreadsContinued"`
			} `json:"body"`
		}
		_ = json.Unmarshal(content, &wire)
		all := wire.Body.AllThreadsContinued
		return &continueResponse{ContinueResponse: response, allThreadsContinued: all == nil || *all}, nil
	}
	if err == nil {
		return message, nil
	}
	var fieldErr *godap.DecodeProtocolMessageFieldError
	if !errors.As(err, &fieldErr) {
		return nil, err
	}
	var generic godap.Message
	switch fieldErr.SubType {
	case "Event":
		generic = &godap.Event{}
	case "Request":
		generic = &godap.Request{}
	case "Response":
		generic = &godap.Response{}
	default:
		return nil, err
	}
	if err := json.Unmarshal(content, generic); err != nil {
		return nil, err
	}
	return generic, nil
}

func (session *Session) handleEvent(message godap.EventMessage) {
	switch event := message.(type) {
	case *godap.InitializedEvent:
		session.initOnce.Do(func() { close(session.initialized) })
	case *godap.StoppedEvent:
		stop := Stop{
			Reason:            event.Body.Reason,
			Description:       event.Body.Description,
			ThreadID:          event.Body.ThreadId,
			AllThreadsStopped: event.Body.AllThreadsStopped,
			HitBreakpointIDs:  session.gatewayBreakpointIDs(event.Body.HitBreakpointIds),
			Sequence:          stopSequence.Add(1),
		}
		session.mu.Lock()
		session.recordStopLocked(stop)
		session.notifyStateLocked()
		parent := session.parent
		session.mu.Unlock()
		if parent != nil {
			parent.focusChild(session)
		}
	case *godap.ContinuedEvent:
		session.mu.Lock()
		session.resumeThreadLocked(event.Body.ThreadId, event.Body.AllThreadsContinued)
		session.notifyStateLocked()
		session.mu.Unlock()
	case *godap.BreakpointEvent:
		if event.Body.Reason != "changed" {
			return
		}
		session.mu.Lock()
		ref, ok := session.adapterIDs[event.Body.Breakpoint.Id]
		session.mu.Unlock()
		if ok && session.hooks.onBreakpoint != nil {
			session.hooks.onBreakpoint(ref.path, ref.line, ref.column, breakpointsFromWire([]godap.Breakpoint{event.Body.Breakpoint})[0])
		}
	case *godap.ExitedEvent:
		exitCode := event.Body.ExitCode
		session.mu.Lock()
		session.exitCode = &exitCode
		session.state = StateTerminated
		session.stop = nil
		session.notifyStateLocked()
		session.mu.Unlock()
		session.closeAfterTermination()
	case *godap.TerminatedEvent:
		session.setState(StateTerminated, nil)
		session.closeAfterTermination()
	case *godap.OutputEvent:
		session.output.append(event.Body.Category, event.Body.Output)
	}
}

// recordStopLocked tracks suspended threads. All-stop adapters suspend every
// thread at once; others stop and resume threads individually.
func (session *Session) recordStopLocked(stop Stop) {
	if stop.AllThreadsStopped || session.state != StateStopped || session.threadStops == nil {
		session.threadStops = map[int]Stop{}
		session.allStopped = stop.AllThreadsStopped
	}
	session.threadStops[stop.ThreadID] = stop
	session.state = StateStopped
	session.stop = &stop
}

func (session *Session) resumeThreadLocked(threadID int, all bool) {
	if session.state == StateStopped && !all && !session.allStopped {
		delete(session.threadStops, threadID)
		if len(session.threadStops) > 0 {
			if session.stop == nil || session.stop.ThreadID == threadID {
				next := session.threadStops[slices.Min(slices.Collect(maps.Keys(session.threadStops)))]
				session.stop = &next
			}
			return
		}
	}
	session.state, session.stop = StateRunning, nil
	session.threadStops, session.allStopped = nil, false
}

func (session *Session) gatewayBreakpointIDs(ids []int) []int {
	if session.hooks.breakpointID == nil {
		return slices.Clone(ids)
	}
	var result []int
	for _, id := range ids {
		session.mu.Lock()
		ref, ok := session.adapterIDs[id]
		session.mu.Unlock()
		if !ok {
			continue
		}
		if gateway := session.hooks.breakpointID(ref.path, ref.line, ref.column); gateway > 0 {
			result = append(result, gateway)
		}
	}
	return result
}

func (session *Session) closeAfterTermination() {
	session.terminateOnce.Do(func() {
		go func() {
			<-session.launchDone
			_ = session.Disconnect(context.Background(), true)
		}()
	})
}

func (session *Session) handleAdapterRequest(message godap.RequestMessage) {
	if request, ok := message.(*godap.StartDebuggingRequest); ok {
		session.respondStartDebugging(request)
		return
	}
	if request, ok := message.(*godap.RunInTerminalRequest); ok {
		session.respondRunInTerminal(request)
		return
	}
	session.respondError(message.GetRequest(), "notSupported", "Wingman does not support adapter-initiated request {command}")
}

func (session *Session) respondStartDebugging(request *godap.StartDebuggingRequest) {
	connector := session.connection.childConnector()
	if connector == nil {
		session.respondError(&request.Request, "notSupported", "The debug adapter does not accept child sessions")
		return
	}
	requestName := strings.ToLower(strings.TrimSpace(request.Arguments.Request))
	if requestName != "launch" && requestName != "attach" {
		session.respondError(&request.Request, "invalidArgs", "startDebugging requires a launch or attach request")
		return
	}
	arguments := make(map[string]any, len(request.Arguments.Configuration)+1)
	maps.Copy(arguments, request.Arguments.Configuration)
	arguments["request"] = requestName
	childPlan := session.plan
	childPlan.Adapter.Transport = TransportConnect
	childPlan.Adapter.Command = ""
	childPlan.Adapter.Args = nil
	childPlan.Request = requestName
	childPlan.Arguments = arguments
	childPlan.PreLaunch = nil

	ctx, cancel := context.WithTimeout(context.Background(), adapterStartupTimeout)
	defer cancel()
	hooks := session.hooks
	hooks.output = session.output
	hooks.breakpointSource = func() (map[string][]SourceBreakpoint, uint64) { return session.breakpointSnapshot(), 0 }
	child, err := startSession(ctx, fmt.Sprintf("%s-child-%d", session.id, request.Seq), childPlan, StartOptions{
		Breakpoints:         session.breakpointSnapshot(),
		FunctionBreakpoints: session.functionBreakpointSnapshot(),
		terminalLauncher:    session.terminalLauncher,
		adapterConnector:    connector,
		hooks:               hooks,
	})
	if err != nil {
		session.respondError(&request.Request, "startDebugging", err.Error())
		return
	}
	if !session.attachChild(child) {
		child.Close()
		session.respondError(&request.Request, "startDebugging", "The parent debug session ended while its child session was starting")
		return
	}
	session.respond(&godap.StartDebuggingResponse{Response: godap.Response{
		ProtocolMessage: godap.ProtocolMessage{Type: "response"},
		RequestSeq:      request.Seq,
		Success:         true,
		Command:         request.Command,
	}})
}

// attachChild transfers ownership only while the parent is still active. A
// child can take long enough to start that the parent is closed concurrently;
// installing it afterward would leak the child adapter and debuggee. Earlier
// children keep running: adapters start one child per target or worker.
func (session *Session) attachChild(child *Session) bool {
	child.mu.Lock()
	child.parent = session
	child.mu.Unlock()

	session.mu.Lock()
	if !session.alive.Load() || session.state == StateTerminated {
		session.mu.Unlock()
		child.mu.Lock()
		if child.parent == session {
			child.parent = nil
		}
		child.mu.Unlock()
		return false
	}
	session.children = append(session.children, child)
	session.child = child
	session.notifyStateLocked()
	session.mu.Unlock()
	return true
}

func (session *Session) focusChild(child *Session) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.child != child && slices.Contains(session.children, child) {
		session.child = child
		session.notifyStateLocked()
	}
}

func (session *Session) removeChild(child *Session) {
	session.mu.Lock()
	defer session.mu.Unlock()
	index := slices.Index(session.children, child)
	if index < 0 {
		return
	}
	session.children = slices.Delete(session.children, index, index+1)
	if session.child == child {
		session.child = nil
		if len(session.children) > 0 {
			session.child = session.children[len(session.children)-1]
		}
	}
	session.notifyStateLocked()
}

func (session *Session) childSessions() []*Session {
	session.mu.Lock()
	defer session.mu.Unlock()
	return slices.Clone(session.children)
}

func (session *Session) childSession() *Session {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.child
}

func cloneSourceBreakpointSets(values map[string][]SourceBreakpoint) map[string][]SourceBreakpoint {
	result := make(map[string][]SourceBreakpoint, len(values))
	for path, breakpoints := range values {
		result[filepath.Clean(path)] = slices.Clone(breakpoints)
	}
	return result
}

func (session *Session) breakpointSnapshot() map[string][]SourceBreakpoint {
	session.mu.Lock()
	defer session.mu.Unlock()
	return cloneSourceBreakpointSets(session.breakpoints)
}

func (session *Session) functionBreakpointSnapshot() []FunctionBreakpoint {
	session.mu.Lock()
	defer session.mu.Unlock()
	return slices.Clone(session.functions)
}

func (session *Session) respondRunInTerminal(request *godap.RunInTerminalRequest) {
	if session.plan.IO != IOTerminal || session.terminalLauncher == nil {
		session.respondError(&request.Request, "notSupported", "Terminal launch is not available")
		return
	}
	if request.Arguments.ArgsCanBeInterpretedByShell {
		session.respondError(&request.Request, "notSupported", "Shell-interpreted terminal arguments are not supported")
		return
	}
	if len(request.Arguments.Args) == 0 || strings.TrimSpace(request.Arguments.Args[0]) == "" {
		session.respondError(&request.Request, "invalidArgs", "runInTerminal requires a command")
		return
	}
	environment, err := terminalEnvironment(request.Arguments.Env)
	if err != nil {
		session.respondError(&request.Request, "invalidArgs", err.Error())
		return
	}
	dir := strings.TrimSpace(request.Arguments.Cwd)
	if dir == "" {
		dir = session.plan.ProjectDir
	}
	process, err := session.terminalLauncher.LaunchTerminal(context.Background(), TerminalLaunch{
		Title: request.Arguments.Title,
		Path:  request.Arguments.Args[0],
		Args:  slices.Clone(request.Arguments.Args[1:]),
		Dir:   dir,
		Env:   environment,
	})
	if err != nil {
		session.respondError(&request.Request, "runInTerminal", err.Error())
		return
	}
	if process == nil {
		session.respondError(&request.Request, "runInTerminal", "Terminal launcher returned no process")
		return
	}
	if !session.trackTerminal(process) {
		session.respondError(&request.Request, "runInTerminal", "Debug session is stopping")
		return
	}
	session.respond(&godap.RunInTerminalResponse{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Type: "response"},
			RequestSeq:      request.Seq,
			Success:         true,
			Command:         request.Command,
		},
		Body: godap.RunInTerminalResponseBody{ProcessId: process.ProcessID()},
	})
}

func terminalEnvironment(values map[string]any) (map[string]*string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]*string, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) == "" || strings.Contains(key, "=") {
			return nil, fmt.Errorf("runInTerminal environment variable name %q is invalid", key)
		}
		if value == nil {
			result[key] = nil
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("runInTerminal environment %q must be a string or null", key)
		}
		textCopy := text
		result[key] = &textCopy
	}
	return result, nil
}

func (session *Session) trackTerminal(process TerminalProcess) bool {
	snapshot, output, cancel := process.Subscribe()
	session.mu.Lock()
	if !session.alive.Load() || session.state == StateTerminated {
		session.mu.Unlock()
		cancel()
		_ = process.Close()
		return false
	}
	session.terminalID = process.ID()
	session.terminals = append(session.terminals, terminalBinding{process: process, cancel: cancel})
	session.mu.Unlock()
	if len(snapshot) > 0 {
		session.output.append("stdout", string(snapshot))
	}
	go func() {
		for chunk := range output {
			session.output.append("stdout", string(chunk))
		}
	}()
	return true
}

func (session *Session) respondError(request *godap.Request, id, format string) {
	session.respond(&godap.ErrorResponse{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Type: "response"},
			RequestSeq:      request.Seq,
			Success:         false,
			Command:         request.Command,
			Message:         id,
		},
		Body: godap.ErrorResponseBody{Error: &godap.ErrorMessage{
			Id:        1,
			Format:    format,
			Variables: map[string]string{"command": request.Command},
			ShowUser:  true,
		}},
	})
}

func (session *Session) respond(response godap.ResponseMessage) {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	session.seq++
	response.GetResponse().Seq = session.seq
	response.GetResponse().Type = "response"
	_ = godap.WriteProtocolMessage(session.connection, response)
}

func (session *Session) watchProcess() {
	if session.connection == nil || session.connection.processDone == nil {
		return
	}
	err, ok := <-session.connection.processDone
	if !ok {
		return
	}
	if session.Status().State == StateTerminated {
		session.closeResourcesWithError(nil)
		return
	}
	if err == nil {
		err = errors.New("debug adapter exited unexpectedly")
	} else {
		err = fmt.Errorf("debug adapter exited: %w", err)
	}
	session.closeResourcesWithError(err)
}

func (session *Session) finish(err error) {
	session.finishOnce.Do(func() {
		session.alive.Store(false)
		session.mu.Lock()
		wasTerminated := session.state == StateTerminated
		if session.state != StateTerminated {
			session.state = StateTerminated
		}
		session.stop = nil
		if err != nil && !wasTerminated && !session.disconnecting.Load() {
			session.terminalErr = err
		}
		session.notifyStateLocked()
		parent := session.parent
		session.mu.Unlock()
		if parent != nil {
			parent.removeChild(session)
		}

		session.pendingMu.Lock()
		pending := session.pending
		session.pending = make(map[int]chan responseResult)
		session.pendingMu.Unlock()
		pendingErr := err
		if pendingErr == nil {
			pendingErr = errSessionClosed
		}
		for _, wait := range pending {
			wait <- responseResult{err: pendingErr}
		}
		if session.resourceDone != nil {
			close(session.resourceDone)
		}
	})
}

func (session *Session) setState(state State, stop *Stop) {
	session.mu.Lock()
	session.state = state
	session.stop = stop
	session.notifyStateLocked()
	session.mu.Unlock()
}

func (session *Session) notifyStateLocked() {
	session.stateVersion++
	close(session.stateChanged)
	session.stateChanged = make(chan struct{})
	if session.parent != nil {
		session.parent.notifyChildStateChanged()
	}
}

func (session *Session) notifyChildStateChanged() {
	session.mu.Lock()
	session.stateVersion++
	close(session.stateChanged)
	session.stateChanged = make(chan struct{})
	parent := session.parent
	session.mu.Unlock()
	if parent != nil {
		parent.notifyChildStateChanged()
	}
}

func (session *Session) waitForStop(ctx context.Context, after uint64) (Status, bool) {
	for {
		session.mu.Lock()
		version := session.stateVersion
		state := session.state
		child := session.child
		changed := session.stateChanged
		session.mu.Unlock()
		if child != nil {
			state = child.Status().State
		}
		if version > after && (state == StateStopped || state == StateTerminated) {
			return session.Status(), true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return session.Status(), false
		}
	}
}

func (session *Session) stateEpoch() uint64 {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.stateVersion
}

func (session *Session) SetBreakpoints(ctx context.Context, path string, values []SourceBreakpoint) ([]Breakpoint, error) {
	if err := validateSourceBreakpoints(values); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	session.mu.Lock()
	if len(values) == 0 {
		delete(session.breakpoints, path)
	} else {
		session.breakpoints[path] = slices.Clone(values)
	}
	children, focused := slices.Clone(session.children), session.child
	session.mu.Unlock()
	if len(children) > 0 {
		var result []Breakpoint
		var resultErr error
		for _, child := range children {
			resolved, err := child.SetBreakpoints(ctx, path, values)
			if child == focused || result == nil && resultErr == nil {
				result, resultErr = resolved, err
			}
		}
		return result, resultErr
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(session.plan.ProjectDir, path)
	}
	path = filepath.Clean(path)
	wire := make([]godap.SourceBreakpoint, 0, len(values))
	for _, value := range values {
		wire = append(wire, godap.SourceBreakpoint{
			Line:         value.Line,
			Column:       value.Column,
			Condition:    value.Condition,
			HitCondition: value.HitCondition,
			LogMessage:   value.LogMessage,
		})
	}
	response, err := session.request(ctx, &godap.SetBreakpointsRequest{
		Request: request("setBreakpoints"),
		Arguments: godap.SetBreakpointsArguments{
			Source:      godap.Source{Name: filepath.Base(path), Path: path},
			Breakpoints: wire,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("set breakpoints in %s: %w", path, err)
	}
	result, ok := response.(*godap.SetBreakpointsResponse)
	if !ok {
		return nil, fmt.Errorf("setBreakpoints returned %T", response)
	}
	resolved := breakpointsFromWire(result.Body.Breakpoints)
	session.mu.Lock()
	for id, ref := range session.adapterIDs {
		if ref.path == path {
			delete(session.adapterIDs, id)
		}
	}
	// Results follow request order; adapter ids are translated through the
	// requested position because adapters may move a verified breakpoint.
	for index, breakpoint := range resolved {
		if breakpoint.ID != 0 && index < len(values) {
			session.adapterIDs[breakpoint.ID] = breakpointRef{path: path, line: values[index].Line, column: values[index].Column}
		}
	}
	session.mu.Unlock()
	return resolved, nil
}

// reportBreakpoints publishes verification from setBreakpoints calls that the
// manager did not issue itself, such as the configuration step.
func (session *Session) reportBreakpoints(path string, requested []SourceBreakpoint, resolved []Breakpoint) {
	if session.hooks.onBreakpoint == nil {
		return
	}
	path = filepath.Clean(path)
	for index, breakpoint := range resolved {
		if index < len(requested) {
			session.hooks.onBreakpoint(path, requested[index].Line, requested[index].Column, breakpoint)
		}
	}
}

func adapterCapabilities(adapter godap.Capabilities) Capabilities {
	capabilities := Capabilities{
		SupportsStepBack:                  adapter.SupportsStepBack,
		SupportsTerminateRequest:          adapter.SupportsTerminateRequest,
		SupportsCancelRequest:             adapter.SupportsCancelRequest,
		SupportsConditionalBreakpoints:    adapter.SupportsConditionalBreakpoints,
		SupportsHitConditionalBreakpoints: adapter.SupportsHitConditionalBreakpoints,
		SupportsLogPoints:                 adapter.SupportsLogPoints,
		SupportsEvaluateForHovers:         adapter.SupportsEvaluateForHovers,
	}
	for _, filter := range adapter.ExceptionBreakpointFilters {
		capabilities.ExceptionFilters = append(capabilities.ExceptionFilters, ExceptionFilter{
			Filter: filter.Filter, Label: filter.Label, Description: filter.Description, Default: filter.Default,
		})
	}
	return capabilities
}

// initialExceptionFilters returns the editor's selection, or the adapter's
// defaults when the editor has not chosen any filters.
func (session *Session) initialExceptionFilters() ([]string, bool) {
	session.mu.Lock()
	available := slices.Clone(session.capabilities.ExceptionFilters)
	session.mu.Unlock()
	if len(available) == 0 {
		return nil, false
	}
	if source := session.hooks.exceptionFilters; source != nil {
		if filters, set := source(); set {
			return filters, true
		}
	}
	filters := []string{}
	for _, filter := range available {
		if filter.Default {
			filters = append(filters, filter.Filter)
		}
	}
	return filters, true
}

func (session *Session) SetExceptionBreakpoints(ctx context.Context, filters []string) error {
	children := session.childSessions()
	if len(children) > 0 {
		var firstErr error
		for _, child := range children {
			if err := child.SetExceptionBreakpoints(ctx, filters); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	if len(session.Status().Capabilities.ExceptionFilters) == 0 {
		return nil
	}
	return session.sendExceptionBreakpoints(ctx, filters)
}

func (session *Session) sendExceptionBreakpoints(ctx context.Context, filters []string) error {
	_, err := session.request(ctx, &godap.SetExceptionBreakpointsRequest{
		Request:   request("setExceptionBreakpoints"),
		Arguments: godap.SetExceptionBreakpointsArguments{Filters: slices.Clone(filters)},
	})
	if err != nil {
		return fmt.Errorf("set exception breakpoints: %w", err)
	}
	return nil
}

func validateSourceBreakpoints(values []SourceBreakpoint) error {
	for index, value := range values {
		if value.Line <= 0 {
			return fmt.Errorf("breakpoint %d line must be a positive 1-based integer", index+1)
		}
		if value.Column < 0 {
			return fmt.Errorf("breakpoint %d column cannot be negative", index+1)
		}
	}
	return nil
}

func (session *Session) SetFunctionBreakpoints(ctx context.Context, values []FunctionBreakpoint) ([]Breakpoint, error) {
	if err := validateFunctionBreakpoints(values); err != nil {
		return nil, err
	}
	session.mu.Lock()
	session.functions = slices.Clone(values)
	children, focused := slices.Clone(session.children), session.child
	session.mu.Unlock()
	if len(children) > 0 {
		var result []Breakpoint
		var resultErr error
		for _, child := range children {
			resolved, err := child.SetFunctionBreakpoints(ctx, values)
			if child == focused || result == nil && resultErr == nil {
				result, resultErr = resolved, err
			}
		}
		return result, resultErr
	}
	wire := make([]godap.FunctionBreakpoint, 0, len(values))
	for _, value := range values {
		wire = append(wire, godap.FunctionBreakpoint{Name: value.Name, Condition: value.Condition, HitCondition: value.HitCondition})
	}
	response, err := session.request(ctx, &godap.SetFunctionBreakpointsRequest{
		Request:   request("setFunctionBreakpoints"),
		Arguments: godap.SetFunctionBreakpointsArguments{Breakpoints: wire},
	})
	if err != nil {
		return nil, fmt.Errorf("set function breakpoints: %w", err)
	}
	result, ok := response.(*godap.SetFunctionBreakpointsResponse)
	if !ok {
		return nil, fmt.Errorf("setFunctionBreakpoints returned %T", response)
	}
	return breakpointsFromWire(result.Body.Breakpoints), nil
}

func validateFunctionBreakpoints(values []FunctionBreakpoint) error {
	for index, value := range values {
		if strings.TrimSpace(value.Name) == "" {
			return fmt.Errorf("function breakpoint %d name is required", index+1)
		}
	}
	return nil
}

func breakpointsFromWire(values []godap.Breakpoint) []Breakpoint {
	result := make([]Breakpoint, 0, len(values))
	for _, value := range values {
		result = append(result, Breakpoint{ID: value.Id, Verified: value.Verified, Message: value.Message, Line: value.Line, Column: value.Column})
	}
	return result
}

func (session *Session) Continue(ctx context.Context, threadID int) error {
	_, err := session.ContinueThread(ctx, threadID)
	return err
}

// ContinueThread reports whether the adapter resumed every thread.
func (session *Session) ContinueThread(ctx context.Context, threadID int) (bool, error) {
	if child := session.childSession(); child != nil {
		return child.ContinueThread(ctx, threadID)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return false, err
	}
	response, epoch, err := session.resume(ctx, threadID, &godap.ContinueRequest{Request: request("continue"), Arguments: godap.ContinueArguments{ThreadId: threadID}})
	if err != nil {
		return false, err
	}
	all := true
	if wire, ok := response.(*continueResponse); ok {
		all = wire.allThreadsContinued
	}
	if all {
		session.mu.Lock()
		if session.state == StateStopped && session.stateVersion == epoch {
			session.resumeThreadLocked(threadID, true)
			session.notifyStateLocked()
		}
		session.mu.Unlock()
	}
	return all, nil
}

func (session *Session) ReverseContinue(ctx context.Context, threadID int) error {
	if child := session.childSession(); child != nil {
		return child.ReverseContinue(ctx, threadID)
	}
	if !session.Status().Capabilities.SupportsStepBack {
		return fmt.Errorf("debug adapter %s does not support reverse execution", session.plan.Adapter.Name)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	return session.resumeRequest(ctx, threadID, &godap.ReverseContinueRequest{Request: request("reverseContinue"), Arguments: godap.ReverseContinueArguments{ThreadId: threadID}})
}

func (session *Session) Next(ctx context.Context, threadID int) error {
	if child := session.childSession(); child != nil {
		return child.Next(ctx, threadID)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	return session.resumeRequest(ctx, threadID, &godap.NextRequest{Request: request("next"), Arguments: godap.NextArguments{ThreadId: threadID}})
}

func (session *Session) StepIn(ctx context.Context, threadID int) error {
	if child := session.childSession(); child != nil {
		return child.StepIn(ctx, threadID)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	return session.resumeRequest(ctx, threadID, &godap.StepInRequest{Request: request("stepIn"), Arguments: godap.StepInArguments{ThreadId: threadID}})
}

func (session *Session) StepOut(ctx context.Context, threadID int) error {
	if child := session.childSession(); child != nil {
		return child.StepOut(ctx, threadID)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	return session.resumeRequest(ctx, threadID, &godap.StepOutRequest{Request: request("stepOut"), Arguments: godap.StepOutArguments{ThreadId: threadID}})
}

func (session *Session) StepBack(ctx context.Context, threadID int) error {
	if child := session.childSession(); child != nil {
		return child.StepBack(ctx, threadID)
	}
	if !session.Status().Capabilities.SupportsStepBack {
		return fmt.Errorf("debug adapter %s does not support stepping backward", session.plan.Adapter.Name)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	return session.resumeRequest(ctx, threadID, &godap.StepBackRequest{
		Request:   request("stepBack"),
		Arguments: godap.StepBackArguments{ThreadId: threadID},
	})
}

func (session *Session) resumeRequest(ctx context.Context, threadID int, message godap.RequestMessage) error {
	_, _, err := session.resume(ctx, threadID, message)
	return err
}

// resume marks the thread running before sending. A fast adapter can emit the
// next stopped event immediately after its response; updating state afterwards
// would race with and overwrite that newer event. The stopped check and
// transition are one critical section for the same reason.
func (session *Session) resume(ctx context.Context, threadID int, message godap.RequestMessage) (godap.ResponseMessage, uint64, error) {
	previous, runningEpoch, err := session.beginResume(message.GetRequest().Command, threadID)
	if err != nil {
		return nil, 0, err
	}
	response, err := session.request(ctx, message)
	if err != nil {
		// A cancelled or timed-out request may already have resumed the debuggee.
		if ctx.Err() == nil {
			session.restoreFailedResume(runningEpoch, previous)
		}
		return nil, 0, err
	}
	return response, runningEpoch, nil
}

type stopState struct {
	state       State
	stop        *Stop
	threadStops map[int]Stop
	allStopped  bool
}

func (session *Session) beginResume(command string, threadID int) (stopState, uint64, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != StateStopped {
		return stopState{}, 0, fmt.Errorf("debug session must be stopped before %s", command)
	}
	previous := stopState{state: session.state, stop: session.stop, threadStops: maps.Clone(session.threadStops), allStopped: session.allStopped}
	if previous.stop != nil {
		clone := *previous.stop
		clone.HitBreakpointIDs = slices.Clone(previous.stop.HitBreakpointIDs)
		previous.stop = &clone
	}
	session.resumeThreadLocked(threadID, false)
	session.notifyStateLocked()
	return previous, session.stateVersion, nil
}

// restoreFailedResume rolls back the optimistic running state only when no
// newer adapter event has arrived. Checking and restoring under one lock keeps
// a stopped or continued event from being overwritten between those steps.
func (session *Session) restoreFailedResume(runningEpoch uint64, previous stopState) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.stateVersion != runningEpoch {
		return
	}
	session.state, session.stop = previous.state, previous.stop
	session.threadStops, session.allStopped = previous.threadStops, previous.allStopped
	session.notifyStateLocked()
}

func (session *Session) Pause(ctx context.Context, threadID int) error {
	if child := session.childSession(); child != nil {
		return child.Pause(ctx, threadID)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	_, err = session.request(ctx, &godap.PauseRequest{Request: request("pause"), Arguments: godap.PauseArguments{ThreadId: threadID}})
	return err
}

func (session *Session) WaitForStop(ctx context.Context, after uint64) (Status, bool) {
	return session.waitForStop(ctx, after)
}

func (session *Session) StateEpoch() uint64 { return session.stateEpoch() }

func (session *Session) Threads(ctx context.Context) ([]Thread, error) {
	if child := session.childSession(); child != nil {
		return child.Threads(ctx)
	}
	response, err := session.request(ctx, &godap.ThreadsRequest{Request: request("threads")})
	if err != nil {
		return nil, err
	}
	result, ok := response.(*godap.ThreadsResponse)
	if !ok {
		return nil, fmt.Errorf("threads returned %T", response)
	}
	threads := make([]Thread, 0, len(result.Body.Threads))
	for _, thread := range result.Body.Threads {
		threads = append(threads, Thread{ID: thread.Id, Name: thread.Name})
	}
	return threads, nil
}

func (session *Session) resolveThreadID(ctx context.Context, threadID int) (int, error) {
	if threadID > 0 {
		return threadID, nil
	}
	status := session.Status()
	if status.Stop != nil && status.Stop.ThreadID > 0 {
		return status.Stop.ThreadID, nil
	}
	threads, err := session.Threads(ctx)
	if err != nil {
		return 0, err
	}
	if len(threads) == 0 {
		return 0, errors.New("debug adapter reported no threads")
	}
	return threads[0].ID, nil
}

func (session *Session) StackTrace(ctx context.Context, threadID, start, levels int) ([]StackFrame, int, error) {
	if child := session.childSession(); child != nil {
		return child.StackTrace(ctx, threadID, start, levels)
	}
	threadID, err := session.resolveThreadID(ctx, threadID)
	if err != nil {
		return nil, 0, err
	}
	response, err := session.request(ctx, &godap.StackTraceRequest{
		Request:   request("stackTrace"),
		Arguments: godap.StackTraceArguments{ThreadId: threadID, StartFrame: start, Levels: levels},
	})
	if err != nil {
		return nil, 0, err
	}
	result, ok := response.(*godap.StackTraceResponse)
	if !ok {
		return nil, 0, fmt.Errorf("stackTrace returned %T", response)
	}
	frames := make([]StackFrame, 0, len(result.Body.StackFrames))
	for _, frame := range result.Body.StackFrames {
		converted := StackFrame{ID: frame.Id, Name: frame.Name, Line: frame.Line, Column: frame.Column}
		if frame.Source != nil {
			converted.Source = &Source{Name: frame.Source.Name, Path: frame.Source.Path}
		}
		frames = append(frames, converted)
	}
	return frames, result.Body.TotalFrames, nil
}

func (session *Session) Scopes(ctx context.Context, frameID int) ([]Scope, error) {
	if child := session.childSession(); child != nil {
		return child.Scopes(ctx, frameID)
	}
	if frameID <= 0 {
		frames, _, err := session.StackTrace(ctx, 0, 0, 1)
		if err != nil {
			return nil, err
		}
		if len(frames) == 0 {
			return nil, errors.New("current thread has no stack frames")
		}
		frameID = frames[0].ID
	}
	response, err := session.request(ctx, &godap.ScopesRequest{Request: request("scopes"), Arguments: godap.ScopesArguments{FrameId: frameID}})
	if err != nil {
		return nil, err
	}
	result, ok := response.(*godap.ScopesResponse)
	if !ok {
		return nil, fmt.Errorf("scopes returned %T", response)
	}
	scopes := make([]Scope, 0, len(result.Body.Scopes))
	for _, scope := range result.Body.Scopes {
		scopes = append(scopes, Scope{
			Name: scope.Name, VariablesReference: scope.VariablesReference,
			NamedVariables: scope.NamedVariables, IndexedVariables: scope.IndexedVariables, Expensive: scope.Expensive,
		})
	}
	return scopes, nil
}

func (session *Session) Variables(ctx context.Context, reference, start, count int) ([]Variable, error) {
	return session.FilteredVariables(ctx, reference, "", start, count)
}

// FilteredVariables passes DAP's "indexed" or "named" filter; zero count means all.
func (session *Session) FilteredVariables(ctx context.Context, reference int, filter string, start, count int) ([]Variable, error) {
	if child := session.childSession(); child != nil {
		return child.FilteredVariables(ctx, reference, filter, start, count)
	}
	if reference <= 0 {
		return nil, errors.New("variables_reference must be a positive integer returned by scopes, variables, or evaluate")
	}
	response, err := session.request(ctx, &godap.VariablesRequest{
		Request:   request("variables"),
		Arguments: godap.VariablesArguments{VariablesReference: reference, Filter: filter, Start: start, Count: count},
	})
	if err != nil {
		return nil, err
	}
	result, ok := response.(*godap.VariablesResponse)
	if !ok {
		return nil, fmt.Errorf("variables returned %T", response)
	}
	variables := make([]Variable, 0, len(result.Body.Variables))
	for _, variable := range result.Body.Variables {
		variables = append(variables, Variable{
			Name: variable.Name, Value: variable.Value, Type: variable.Type, EvaluateName: variable.EvaluateName,
			VariablesReference: variable.VariablesReference, NamedVariables: variable.NamedVariables, IndexedVariables: variable.IndexedVariables,
		})
	}
	return variables, nil
}

func (session *Session) Evaluate(ctx context.Context, expression string, frameID int) (Evaluation, error) {
	return session.EvaluateContext(ctx, expression, frameID, "repl")
}

// EvaluateContext evaluates an expression using a standard DAP context such
// as "repl", "hover", or "watch". Adapters may format or restrict results
// differently for each context.
func (session *Session) EvaluateContext(ctx context.Context, expression string, frameID int, evaluationContext string) (Evaluation, error) {
	if child := session.childSession(); child != nil {
		return child.EvaluateContext(ctx, expression, frameID, evaluationContext)
	}
	if strings.TrimSpace(expression) == "" {
		return Evaluation{}, errors.New("expression is required")
	}
	evaluationContext = strings.TrimSpace(evaluationContext)
	if evaluationContext == "" {
		evaluationContext = "repl"
	}
	// Without a frame, DAP evaluates in the global scope.
	frameID = max(frameID, 0)
	response, err := session.request(ctx, &godap.EvaluateRequest{
		Request:   request("evaluate"),
		Arguments: godap.EvaluateArguments{Expression: expression, FrameId: frameID, Context: evaluationContext},
	})
	if err != nil {
		return Evaluation{}, err
	}
	result, ok := response.(*godap.EvaluateResponse)
	if !ok {
		return Evaluation{}, fmt.Errorf("evaluate returned %T", response)
	}
	return Evaluation{
		Result: result.Body.Result, Type: result.Body.Type, VariablesReference: result.Body.VariablesReference,
		NamedVariables: result.Body.NamedVariables, IndexedVariables: result.Body.IndexedVariables,
	}, nil
}

func (session *Session) Disconnect(ctx context.Context, terminate bool) error {
	// Process and connection watchers can finish concurrently with a user stop.
	// Publish ownership intent before either side observes process exit so a
	// forced close is not surfaced as an unexpected adapter crash.
	session.disconnecting.Store(true)
	session.disconnectMu.Lock()
	defer session.disconnectMu.Unlock()
	for _, child := range session.childSessions() {
		_ = child.Disconnect(ctx, terminate)
	}

	if !session.alive.Load() {
		session.closeResources()
		return nil
	}

	// Stop is a local ownership boundary: mark the session terminated before
	// asking the adapter so a clean adapter exit cannot be mistaken for a crash.
	session.setState(StateTerminated, nil)
	disconnectCtx, cancel := context.WithTimeout(ctx, disconnectTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := session.request(disconnectCtx, &godap.DisconnectRequest{
			Request:   request("disconnect"),
			Arguments: &godap.DisconnectArguments{TerminateDebuggee: terminate},
		})
		result <- err
	}()

	var requestErr error
	select {
	case requestErr = <-result:
	case <-disconnectCtx.Done():
		requestErr = disconnectCtx.Err()
	}
	if requestErr == nil {
		grace := time.NewTimer(adapterExitGracePeriod)
		defer grace.Stop()
		select {
		case <-session.resourceDone:
			return nil
		case <-grace.C:
		case <-disconnectCtx.Done():
		}
	}
	session.closeResources()
	if errors.Is(requestErr, context.Canceled) || errors.Is(requestErr, context.DeadlineExceeded) {
		// The graceful handshake timed out, but all locally owned adapter,
		// debuggee, and terminal resources were forcibly closed.
		return nil
	}
	return requestErr
}

// Terminate asks the debuggee to end gracefully. The adapter's terminated
// event then closes the session. It reports false when the adapter lacks the
// terminate request, so the caller can disconnect instead.
func (session *Session) Terminate(ctx context.Context) (bool, error) {
	session.mu.Lock()
	supported := session.adapter.SupportsTerminateRequest
	session.mu.Unlock()
	if !supported || !session.alive.Load() {
		return false, nil
	}
	_, err := session.request(ctx, &godap.TerminateRequest{Request: request("terminate"), Arguments: &godap.TerminateArguments{}})
	return true, err
}

func (session *Session) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = session.Disconnect(ctx, true)
}

func (session *Session) closeResources() {
	session.closeResourcesWithError(errSessionClosed)
}

func (session *Session) closeResourcesWithError(closeErr error) {
	session.closeOnce.Do(func() {
		session.launchOnce.Do(func() { close(session.launchDone) })
		session.alive.Store(false)
		if session.connection != nil {
			_ = session.connection.Close()
		}
		if session.preLaunch != nil {
			session.preLaunch.Close()
		}
		session.mu.Lock()
		children := session.children
		session.child, session.children = nil, nil
		terminals := slices.Clone(session.terminals)
		session.terminals = nil
		session.mu.Unlock()
		for _, child := range children {
			child.mu.Lock()
			if child.parent == session {
				child.parent = nil
			}
			child.mu.Unlock()
			child.Close()
		}
		for _, terminal := range terminals {
			terminal.cancel()
			_ = terminal.process.Close()
		}
		session.finish(closeErr)
	})
}
