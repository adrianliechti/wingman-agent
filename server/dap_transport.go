package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/dap"
)

var (
	errDebugNotStopped = errors.New("debug session must be stopped to inspect it")
	errDAPUnsupported  = errors.New("unsupported DAP command")
)

type dapBrowserRequest struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments"`
}
type dapReference struct {
	native int
	kind   string
}
type dapEpoch struct {
	session string
	version uint64
}
type dapJob struct {
	req  dapBrowserRequest
	ctx  context.Context
	done func()
}
type dapConnection struct {
	*editorSocket
	server     *Server
	outMu      sync.Mutex
	sequence   int
	mu         sync.Mutex
	configured chan struct{}
	bound      string
	epoch      dapEpoch
	references map[int]dapReference
	ids        map[dapReference]int
}

func (s *Server) handleDAP(w http.ResponseWriter, r *http.Request) {
	peer, err := s.acceptEditorSocket(w, r)
	if err != nil {
		return
	}
	defer peer.close()
	c := &dapConnection{editorSocket: peer, server: s, references: map[int]dapReference{}, ids: map[dapReference]int{}}
	// Requests run in order, so the last setBreakpoints for a file wins. Launch
	// waits for configurationDone and evaluation can be slow; both run aside.
	queue := make(chan dapJob, 128)
	go func() {
		for {
			select {
			case <-peer.ctx.Done():
				return
			case job := <-queue:
				c.run(job)
			}
		}
	}()
	initialized := false
	for {
		_, data, err := peer.writer.conn.Read(peer.ctx)
		if err != nil {
			return
		}
		var req dapBrowserRequest
		if json.Unmarshal(data, &req) != nil || req.Type != "request" || req.Seq <= 0 || req.Command == "" {
			peer.cancel()
			return
		}
		if req.Command == "cancel" {
			var p struct {
				RequestID int `json:"requestId"`
			}
			_ = json.Unmarshal(req.Arguments, &p)
			peer.cancelRequest(strconv.Itoa(p.RequestID))
			c.reply(req, map[string]any{}, nil)
			continue
		}
		if req.Command == "initialize" {
			if initialized {
				c.reply(req, nil, errors.New("already initialized"))
				continue
			}
			var p struct {
				Lines      *bool  `json:"linesStartAt1"`
				Columns    *bool  `json:"columnsStartAt1"`
				PathFormat string `json:"pathFormat"`
			}
			_ = json.Unmarshal(req.Arguments, &p)
			if (p.Lines != nil && !*p.Lines) || (p.Columns != nil && !*p.Columns) || (p.PathFormat != "" && p.PathFormat != "path") {
				c.reply(req, nil, errors.New("Wingman DAP requires one-based positions and native paths"))
				continue
			}
			initialized = true
			// Breakpoint and evaluation features depend on the adapter and arrive
			// in a capabilities event once a session is known.
			c.reply(req, map[string]any{"supportsConfigurationDoneRequest": true, "supportsCancelRequest": true, "supportTerminateDebuggee": true, "supportsTerminateRequest": true, "_wingman": map[string]any{"workspacePath": s.workspace.RootPath, "scope": s.scope, "persistentSession": true}}, nil)
			go c.observe()
			continue
		}
		if !initialized {
			c.reply(req, nil, errors.New("initialize is required"))
			continue
		}
		timeout := 2 * time.Minute
		if req.Command == "launch" || req.Command == "attach" {
			timeout = 0 // prelaunch builds and adapter startup carry their own limits
		}
		ctx, done, err := peer.begin(strconv.Itoa(req.Seq), timeout)
		if err != nil {
			c.reply(req, nil, err)
			continue
		}
		job := dapJob{req: req, ctx: ctx, done: done}
		if req.Command == "launch" || req.Command == "attach" || req.Command == "evaluate" {
			go c.run(job)
			continue
		}
		select {
		case queue <- job:
		default:
			done()
			peer.cancel()
			return
		}
	}
}

func (c *dapConnection) run(job dapJob) {
	defer job.done()
	result, err := c.request(job.ctx, job.req)
	c.reply(job.req, result, editorRequestError(job.ctx, err))
}

func (c *dapConnection) sendDAP(value map[string]any) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.sequence++
	value["seq"] = c.sequence
	c.send(value)
}

// reply puts DAP's short error code in message and the readable text in
// body.error, which clients show to the user.
func (c *dapConnection) reply(req dapBrowserRequest, result any, err error) {
	message := map[string]any{"type": "response", "request_seq": req.Seq, "command": req.Command, "success": err == nil}
	if err != nil {
		code := "failed"
		switch {
		case errors.Is(err, errEditorCancelled):
			code = "cancelled"
		case errors.Is(err, errEditorTimeout):
			code = "timeout"
		case errors.Is(err, errDebugNotStopped):
			code = "notStopped"
		case errors.Is(err, errDebugStateChanged):
			code = "stateChanged"
		case errors.Is(err, errDAPUnsupported):
			code = "notSupported"
		}
		message["message"] = code
		message["body"] = map[string]any{"error": map[string]any{"id": 1, "format": err.Error(), "showUser": code != "cancelled"}}
	} else {
		if result == nil {
			result = map[string]any{}
		}
		message["body"] = result
	}
	c.sendDAP(message)
}
func (c *dapConnection) event(event string, body any) {
	c.sendDAP(map[string]any{"type": "event", "event": event, "body": body})
}

// sourcePath confines a source to the workspace; allowMissing accepts a deleted file.
func (c *dapConnection) sourcePath(value string, allowMissing bool) (string, error) {
	if filepath.IsAbs(value) {
		rel, err := filepath.Rel(c.server.workspace.RootPath, value)
		if err != nil {
			return "", err
		}
		value = filepath.ToSlash(rel)
	}
	rel, ok := c.server.workspaceRel(value)
	if !ok {
		return "", errors.New("source is outside the workspace")
	}
	info, err := c.server.workspace.Root.Stat(rel)
	if allowMissing && errors.Is(err, fs.ErrNotExist) {
		return filepath.Join(c.server.workspace.RootPath, rel), nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("source must be a regular file")
	}
	return filepath.Join(c.server.workspace.RootPath, rel), nil
}

// framePath reports adapter paths through the workspace's own spelling;
// adapters often resolve symlinks such as macOS /var -> /private/var.
func (c *dapConnection) framePath(path, resolvedRoot string) string {
	root := c.server.workspace.RootPath
	if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
		return path
	}
	resolved, err := pathutil.Resolve(filepath.Clean(path))
	if err != nil || resolvedRoot == "" {
		return path
	}
	if rel, err := filepath.Rel(resolvedRoot, resolved); err == nil && filepath.IsLocal(rel) {
		return filepath.Join(root, rel)
	}
	return path
}

// reference maps an adapter id into this connection's id space. Ids belong to
// one stop of one session; a newer stop discards every earlier id.
func (c *dapConnection) reference(status dap.Status, native int, kind string) (int, error) {
	if native <= 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	epoch := dapEpoch{status.SessionID, status.StateVersion}
	if epoch != c.epoch {
		if epoch.session == c.epoch.session && epoch.version < c.epoch.version {
			return 0, errDebugStateChanged
		}
		c.epoch = epoch
		clear(c.references)
		clear(c.ids)
	}
	ref := dapReference{native, kind}
	if id, ok := c.ids[ref]; ok {
		return id, nil
	}
	id := int(c.server.dapReferenceSeq.Add(1))
	if len(c.references) >= 65536 || id >= 1<<31-1 {
		return 0, errors.New("debug reference limit reached; reconnect")
	}
	if c.references == nil || c.ids == nil {
		c.references, c.ids = map[int]dapReference{}, map[dapReference]int{}
	}
	c.references[id] = ref
	c.ids[ref] = id
	return id, nil
}
func (c *dapConnection) nativeReference(status dap.Status, id int, kind string) (int, error) {
	c.mu.Lock()
	ref, exists := c.references[id]
	current := c.epoch == dapEpoch{status.SessionID, status.StateVersion}
	c.mu.Unlock()
	if !exists || !current || ref.kind != kind || status.State != dap.StateStopped {
		return 0, errDebugStateChanged
	}
	return ref.native, nil
}

func (c *dapConnection) request(ctx context.Context, req dapBrowserRequest) (any, error) {
	var p struct {
		Source      dap.Source `json:"source"`
		Breakpoints []struct {
			Line         int    `json:"line"`
			Column       int    `json:"column"`
			Condition    string `json:"condition"`
			HitCondition string `json:"hitCondition"`
			LogMessage   string `json:"logMessage"`
		} `json:"breakpoints"`
		Filters      []string         `json:"filters"`
		ThreadID     int              `json:"threadId"`
		FrameID      int              `json:"frameId"`
		Reference    int              `json:"variablesReference"`
		Filter       string           `json:"filter"`
		Start        int              `json:"start"`
		Count        int              `json:"count"`
		StartFrame   int              `json:"startFrame"`
		Levels       int              `json:"levels"`
		Expression   string           `json:"expression"`
		Context      string           `json:"context"`
		SessionID    string           `json:"_wingmanSessionId"`
		StateVersion uint64           `json:"_wingmanStateVersion"`
		PlanID       string           `json:"_wingmanPlanId"`
		Plan         *debugLaunchPlan `json:"_wingmanPlan"`
		Terminate    *bool            `json:"terminateDebuggee"`
	}
	if len(req.Arguments) > 0 {
		if err := json.Unmarshal(req.Arguments, &p); err != nil {
			return nil, errors.New("invalid DAP arguments")
		}
	}
	if req.Command == "configurationDone" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.configured == nil {
			return nil, errors.New("no launch is waiting for configuration")
		}
		select {
		case <-c.configured:
		default:
			close(c.configured)
		}
		return nil, nil
	}
	var plan debugLaunchPlan
	if req.Command == "launch" || req.Command == "attach" {
		if p.PlanID == "" {
			return nil, errors.New("launch requires the _wingmanPlanId of a reviewed plan")
		}
		if err := c.server.workspace.WithDAPManager(func(manager *dap.Manager) error {
			var err error
			plan, err = c.server.reviewedDebugPlan(manager, p.PlanID, p.Plan)
			return err
		}); err != nil {
			return nil, err
		}
		if plan.Request != req.Command {
			return nil, fmt.Errorf("the reviewed plan uses %s, not %s", plan.Request, req.Command)
		}
		c.mu.Lock()
		if c.configured != nil {
			c.mu.Unlock()
			return nil, errors.New("launch already in progress")
		}
		configured := make(chan struct{})
		c.configured = configured
		c.mu.Unlock()
		defer func() { c.mu.Lock(); c.configured = nil; c.mu.Unlock() }()
		c.event("initialized", map[string]any{})
		select {
		case <-configured:
		case <-ctx.Done():
			select {
			case <-configured:
			default:
				return nil, ctx.Err()
			}
		}
		// A configured launch outlives its socket; only cancel or shutdown stops it.
		requestCtx := ctx
		var stop context.CancelFunc
		ctx, stop = context.WithCancel(c.server.ctx)
		defer stop()
		defer context.AfterFunc(requestCtx, func() {
			if errors.Is(context.Cause(requestCtx), errEditorCancelled) {
				stop()
			}
		})()
	}
	var result any
	err := c.server.workspace.WithDAPManager(func(manager *dap.Manager) error {
		switch req.Command {
		case "launch", "attach":
			adapters, err := manager.Adapters(ctx)
			if err != nil {
				return err
			}
			if err := c.server.validateDebugPlan(&plan, adapters); err != nil {
				return err
			}
			session, err := manager.Start(ctx, c.server.debugStartOptions(plan))
			if err != nil {
				return err
			}
			c.mu.Lock()
			c.bound = session.ID()
			c.mu.Unlock()
			return nil
		case "wingman/status":
			body := map[string]any{"output": "", "_wingmanOutputOffset": 0, "breakpoints": []dap.SourceBreakpoint{}, "resolved": []dap.Breakpoint{}}
			if p.Source.Path != "" {
				path, err := c.sourcePath(p.Source.Path, true)
				if err != nil {
					return err
				}
				body["breakpoints"] = nonNilSourceBreakpoints(manager.Breakpoints(path))
				body["resolved"] = manager.ResolvedSourceBreakpoints(path)
			}
			if session := manager.ActiveSession(); session != nil {
				body["session"] = session.Status()
				body["output"], body["_wingmanOutputOffset"] = session.OutputSnapshot()
				c.mu.Lock()
				c.bound = session.ID()
				c.mu.Unlock()
			}
			result = body
			return nil
		case "setBreakpoints":
			path, err := c.sourcePath(p.Source.Path, len(p.Breakpoints) == 0)
			if err != nil {
				return err
			}
			values := make([]dap.SourceBreakpoint, 0, len(p.Breakpoints))
			for _, point := range p.Breakpoints {
				values = append(values, dap.SourceBreakpoint{Line: point.Line, Column: point.Column, Condition: point.Condition, HitCondition: point.HitCondition, LogMessage: point.LogMessage})
			}
			resolved, err := manager.SetBreakpoints(ctx, path, values)
			if err != nil {
				return err
			}
			result = map[string]any{"breakpoints": nonNilDAPBreakpoints(resolved)}
			return nil
		case "setExceptionBreakpoints":
			if p.Filters == nil {
				p.Filters = []string{}
			}
			if err := manager.SetExceptionBreakpoints(ctx, p.Filters); err != nil {
				return err
			}
			result = map[string]any{}
			return nil
		}
		c.mu.Lock()
		bound := c.bound
		c.mu.Unlock()
		if p.SessionID != "" {
			bound = p.SessionID
		}
		if bound == "" {
			return errors.New("no debug session is bound; launch or restore its status first")
		}
		session, err := manager.Session(bound)
		if err != nil {
			return err
		}
		before := session.Status()
		if p.StateVersion != 0 && p.StateVersion != before.StateVersion {
			return errDebugStateChanged
		}
		// Evaluation without a frame uses the global scope and may run while
		// the debuggee is running.
		inspect := req.Command == "stackTrace" || req.Command == "scopes" || req.Command == "variables" || req.Command == "evaluate" && p.FrameID != 0
		if inspect && before.State != dap.StateStopped {
			return errDebugNotStopped
		}
		switch req.Command {
		case "continue":
			all, callErr := session.ContinueThread(ctx, p.ThreadID)
			err = callErr
			result = map[string]any{"allThreadsContinued": all}
		case "next", "stepIn", "stepOut", "stepBack", "reverseContinue", "pause":
			methods := map[string]func(context.Context, int) error{"next": session.Next, "stepIn": session.StepIn, "stepOut": session.StepOut, "stepBack": session.StepBack, "reverseContinue": session.ReverseContinue, "pause": session.Pause}
			err = methods[req.Command](ctx, p.ThreadID)
		case "disconnect":
			// DAP ends a launched debuggee by default and leaves an attached one running.
			terminate := before.Request != "attach"
			if p.Terminate != nil {
				terminate = *p.Terminate
			}
			return manager.Disconnect(ctx, bound, terminate)
		case "terminate":
			return manager.Terminate(ctx, bound)
		case "threads":
			threads, callErr := session.Threads(ctx)
			err = callErr
			if threads == nil {
				threads = []dap.Thread{}
			}
			result = map[string]any{"threads": threads}
		case "stackTrace":
			if p.StartFrame < 0 || p.Levels < 0 {
				return errors.New("invalid stack range")
			}
			frames, total, callErr := session.StackTrace(ctx, p.ThreadID, p.StartFrame, p.Levels)
			if callErr != nil {
				return callErr
			}
			if frames == nil {
				frames = []dap.StackFrame{}
			}
			resolvedRoot, _ := pathutil.Resolve(filepath.Clean(c.server.workspace.RootPath))
			for i := range frames {
				frames[i].ID, err = c.reference(before, frames[i].ID, "frame")
				if err != nil {
					return err
				}
				if source := frames[i].Source; source != nil && source.Path != "" {
					source.Path = c.framePath(source.Path, resolvedRoot)
				}
			}
			body := map[string]any{"stackFrames": frames}
			if total > 0 {
				body["totalFrames"] = total
			}
			result = body
		case "scopes":
			id, err := c.nativeReference(before, p.FrameID, "frame")
			if err != nil {
				return err
			}
			scopes, err := session.Scopes(ctx, id)
			if err != nil {
				return err
			}
			values := []any{}
			for _, scope := range scopes {
				id, err := c.reference(before, scope.VariablesReference, "variable")
				if err != nil {
					return err
				}
				values = append(values, map[string]any{"name": scope.Name, "variablesReference": id, "namedVariables": scope.NamedVariables, "indexedVariables": scope.IndexedVariables, "expensive": scope.Expensive})
			}
			result = map[string]any{"scopes": values}
		case "variables":
			id, err := c.nativeReference(before, p.Reference, "variable")
			if err != nil {
				return err
			}
			if p.Start < 0 || p.Count < 0 {
				return errors.New("invalid variable range")
			}
			if p.Filter != "" && p.Filter != "indexed" && p.Filter != "named" {
				return errors.New("variables filter must be indexed or named")
			}
			variables, err := session.FilteredVariables(ctx, id, p.Filter, p.Start, p.Count)
			if err != nil {
				return err
			}
			values := []any{}
			for _, variable := range variables {
				id, err := c.reference(before, variable.VariablesReference, "variable")
				if err != nil {
					return err
				}
				values = append(values, map[string]any{"name": variable.Name, "value": variable.Value, "type": variable.Type, "evaluateName": variable.EvaluateName, "variablesReference": id, "namedVariables": variable.NamedVariables, "indexedVariables": variable.IndexedVariables})
			}
			result = map[string]any{"variables": values}
		case "evaluate":
			frameID := 0
			if p.FrameID != 0 {
				frameID, err = c.nativeReference(before, p.FrameID, "frame")
				if err != nil {
					return err
				}
			}
			value, err := session.EvaluateContext(ctx, p.Expression, frameID, p.Context)
			if err != nil {
				return err
			}
			state := before
			if !inspect {
				state = session.Status()
			}
			id := 0
			if state.State == dap.StateStopped {
				if id, err = c.reference(state, value.VariablesReference, "variable"); err != nil {
					return err
				}
			}
			result = map[string]any{"result": value.Result, "type": value.Type, "variablesReference": id, "namedVariables": value.NamedVariables, "indexedVariables": value.IndexedVariables}
		default:
			return fmt.Errorf("%w: %s", errDAPUnsupported, req.Command)
		}
		if inspect && debugStatusChanged(before, session.Status()) {
			return errDebugStateChanged
		}
		return err
	})
	return result, err
}

// dapObserver remembers what one connection has been told, so every
// lifecycle transition, output chunk and verification is sent exactly once.
type dapObserver struct {
	status        *dap.Status
	stop          uint64
	exited        bool
	terminated    bool
	outputSession string
	outputOffset  int64
	capabilities  string
	breakpoints   map[int]dap.ResolvedBreakpoint
}

func (c *dapConnection) observe() {
	var o dapObserver
	first := true
	for {
		var changed, stateChanged, outputChanged <-chan struct{}
		var session *dap.Session
		var status *dap.Status
		var resolved map[int]dap.ResolvedBreakpoint
		err := c.server.workspace.WithDAPManager(func(manager *dap.Manager) error {
			changed = manager.Changes()
			resolved = manager.ResolvedBreakpoints()
			if session = manager.ActiveSession(); session != nil {
				stateChanged, outputChanged = session.Changes()
				current := session.Status()
				status = &current
			}
			return nil
		})
		if err != nil {
			return
		}
		if first {
			// A new connection restores through wingman/status. Replaying lifecycle
			// events would end a session this client never saw end.
			first = false
			o.status, o.breakpoints = status, resolved
			if status != nil {
				o.terminated = status.State == dap.StateTerminated
				o.exited = o.terminated && status.ExitCode != nil
				if status.Stop != nil {
					o.stop = status.Stop.Sequence
				}
				o.outputSession = status.SessionID
				_, o.outputOffset = session.OutputSnapshot()
				c.sendCapabilities(&o, status.Capabilities)
			}
		} else {
			c.sendOutput(&o, session, status)
			c.sendLifecycle(&o, status)
			c.sendBreakpoints(&o, resolved)
		}
		select {
		case <-c.ctx.Done():
			return
		case <-changed:
		case <-stateChanged:
		case <-outputChanged:
		}
	}
}

func (c *dapConnection) sendOutput(o *dapObserver, session *dap.Session, status *dap.Status) {
	if session == nil {
		return
	}
	if status.SessionID != o.outputSession {
		o.outputSession, o.outputOffset = status.SessionID, 0
	}
	chunks, end := session.OutputSince(o.outputOffset)
	for i := 0; i < len(chunks); {
		// Adjacent chunks of one category travel as one event.
		output, j := chunks[i].Output, i+1
		for ; j < len(chunks) && chunks[j].Category == chunks[i].Category; j++ {
			output += chunks[j].Output
		}
		c.event("output", map[string]any{"category": chunks[i].Category, "output": output, "_wingmanOffset": chunks[i].Offset, "_wingmanSessionId": status.SessionID})
		i = j
	}
	o.outputOffset = end
}

func (c *dapConnection) sendLifecycle(o *dapObserver, status *dap.Status) {
	previous := o.status
	o.status = status
	if status == nil {
		if previous != nil {
			c.event("wingman/status", map[string]any{"session": nil})
			if !o.terminated {
				c.event("terminated", map[string]any{})
			}
		}
		o.exited, o.terminated, o.stop = false, false, 0
		return
	}
	if previous != nil && previous.SessionID != status.SessionID {
		previous = nil
		o.exited, o.terminated, o.stop = false, false, 0
	}
	if previous == nil || debugStatusChanged(*previous, *status) {
		c.event("wingman/status", map[string]any{"session": status})
	}
	c.sendCapabilities(o, status.Capabilities)
	wasStopped := previous != nil && previous.State == dap.StateStopped
	switch status.State {
	case dap.StateStopped:
		if wasStopped {
			for _, thread := range previous.StoppedThreads {
				if !slices.Contains(status.StoppedThreads, thread) {
					c.event("continued", map[string]any{"threadId": thread, "allThreadsContinued": false})
				}
			}
		}
		if stop := status.Stop; stop != nil && stop.Sequence != o.stop {
			o.stop = stop.Sequence
			c.event("stopped", map[string]any{"reason": stop.Reason, "description": stop.Description, "threadId": stop.ThreadID, "allThreadsStopped": stop.AllThreadsStopped, "hitBreakpointIds": nonNilInts(stop.HitBreakpointIDs)})
		} else if stop == nil && !wasStopped {
			c.event("stopped", map[string]any{"reason": "pause"})
		}
	case dap.StateRunning:
		if wasStopped {
			body := map[string]any{"allThreadsContinued": true}
			if previous.Stop != nil {
				body["threadId"] = previous.Stop.ThreadID
			}
			c.event("continued", body)
		}
	case dap.StateTerminated:
		if status.ExitCode != nil && !o.exited {
			o.exited = true
			c.event("exited", map[string]any{"exitCode": *status.ExitCode})
		}
		if !o.terminated {
			o.terminated = true
			c.event("terminated", map[string]any{})
		}
	}
}

// sendCapabilities forwards the adapter features the gateway passes through,
// once per change.
func (c *dapConnection) sendCapabilities(o *dapObserver, capabilities dap.Capabilities) {
	filters := []any{}
	for _, filter := range capabilities.ExceptionFilters {
		filters = append(filters, map[string]any{"filter": filter.Filter, "label": filter.Label, "description": filter.Description, "default": filter.Default})
	}
	body := map[string]any{
		"supportsStepBack":                  capabilities.SupportsStepBack,
		"supportsConditionalBreakpoints":    capabilities.SupportsConditionalBreakpoints,
		"supportsHitConditionalBreakpoints": capabilities.SupportsHitConditionalBreakpoints,
		"supportsLogPoints":                 capabilities.SupportsLogPoints,
		"supportsEvaluateForHovers":         capabilities.SupportsEvaluateForHovers,
		"exceptionBreakpointFilters":        filters,
	}
	data, _ := json.Marshal(body)
	if string(data) != o.capabilities {
		o.capabilities = string(data)
		c.event("capabilities", map[string]any{"capabilities": body})
	}
}

func (c *dapConnection) sendBreakpoints(o *dapObserver, resolved map[int]dap.ResolvedBreakpoint) {
	for _, id := range slices.Sorted(maps.Keys(resolved)) {
		breakpoint := resolved[id]
		if previous, ok := o.breakpoints[id]; ok && reflect.DeepEqual(previous, breakpoint) {
			continue
		}
		wire := map[string]any{"id": id, "verified": breakpoint.Verified, "source": map[string]any{"path": breakpoint.Path}}
		if breakpoint.Message != "" {
			wire["message"] = breakpoint.Message
		}
		if breakpoint.Line > 0 {
			wire["line"] = breakpoint.Line
		}
		if breakpoint.Column > 0 {
			wire["column"] = breakpoint.Column
		}
		if breakpoint.Reason != "" {
			wire["reason"] = breakpoint.Reason
		}
		c.event("breakpoint", map[string]any{"reason": "changed", "breakpoint": wire})
	}
	o.breakpoints = resolved
}

func nonNilInts(values []int) []int {
	if values == nil {
		return []int{}
	}
	return values
}
