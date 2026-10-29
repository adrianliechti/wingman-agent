package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
)

type dapBrowserRequest struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments"`
}
type dapReference struct {
	session string
	version uint64
	native  int
	kind    string
}
type dapConnection struct {
	*editorSocket
	server     *Server
	outMu      sync.Mutex
	sequence   int
	mu         sync.Mutex
	configured chan struct{}
	bound      string
	references map[int]dapReference
}

func (s *Server) handleDAP(w http.ResponseWriter, r *http.Request) {
	peer, err := s.acceptEditorSocket(w, r)
	if err != nil {
		return
	}
	defer peer.close()
	c := &dapConnection{editorSocket: peer, server: s, references: map[int]dapReference{}}
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
			c.reply(req, map[string]any{"supportsConfigurationDoneRequest": true, "supportsCancelRequest": true, "supportsConditionalBreakpoints": true, "supportsHitConditionalBreakpoints": true, "supportsLogPoints": true, "supportsEvaluateForHovers": true, "_wingman": map[string]any{"workspacePath": s.workspace.RootPath, "scope": s.scope, "persistentSession": true}}, nil)
			go c.observe()
			continue
		}
		if !initialized {
			c.reply(req, nil, errors.New("initialize is required"))
			continue
		}
		ctx, done, err := peer.begin(strconv.Itoa(req.Seq))
		if err != nil {
			c.reply(req, nil, err)
			continue
		}
		go func() { defer done(); result, err := c.request(ctx, req); c.reply(req, result, err) }()
	}
}

func (c *dapConnection) sendDAP(value map[string]any) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.sequence++
	value["seq"] = c.sequence
	c.send(value)
}
func (c *dapConnection) reply(req dapBrowserRequest, result any, err error) {
	message := map[string]any{"type": "response", "request_seq": req.Seq, "command": req.Command, "success": err == nil}
	if err != nil {
		message["message"] = err.Error()
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

func (c *dapConnection) sourcePath(value string) (string, error) {
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
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("source must be a regular file")
	}
	return filepath.Join(c.server.workspace.RootPath, rel), nil
}

func (c *dapConnection) reference(status dap.Status, native int, kind string) (int, error) {
	if native <= 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, value := range c.references {
		if value.session != status.SessionID || value.version != status.StateVersion {
			delete(c.references, key)
		}
	}
	ref := dapReference{status.SessionID, status.StateVersion, native, kind}
	for key, value := range c.references {
		if value == ref {
			return key, nil
		}
	}
	id := int(c.server.dapReferenceSeq.Add(1))
	if len(c.references) >= 65536 || id >= 1<<31-1 {
		return 0, errors.New("debug reference limit reached; reconnect")
	}
	c.references[id] = ref
	return id, nil
}
func (c *dapConnection) nativeReference(status dap.Status, id int, kind string) (int, error) {
	c.mu.Lock()
	ref, exists := c.references[id]
	c.mu.Unlock()
	if !exists || ref.kind != kind || ref.session != status.SessionID || ref.version != status.StateVersion || status.State != dap.StateStopped {
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
		ThreadID     int              `json:"threadId"`
		FrameID      int              `json:"frameId"`
		Reference    int              `json:"variablesReference"`
		Start        int              `json:"start"`
		Count        int              `json:"count"`
		StartFrame   int              `json:"startFrame"`
		Levels       int              `json:"levels"`
		Expression   string           `json:"expression"`
		Context      string           `json:"context"`
		SessionID    string           `json:"_wingmanSessionId"`
		StateVersion uint64           `json:"_wingmanStateVersion"`
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
	if req.Command == "launch" || req.Command == "attach" {
		if p.Plan == nil || p.Plan.Request != req.Command {
			return nil, errors.New("launch requires a matching reviewed _wingmanPlan")
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
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-configured:
		}
	}
	var result any
	err := c.server.workspace.WithDAPManager(func(manager *dap.Manager) error {
		switch req.Command {
		case "launch", "attach":
			adapters, err := manager.Adapters(ctx)
			if err != nil {
				return err
			}
			if err := c.server.validateDebugPlan(p.Plan, adapters); err != nil {
				return err
			}
			session, err := manager.Start(ctx, c.server.debugStartOptions(*p.Plan))
			if err != nil {
				return err
			}
			c.mu.Lock()
			c.bound = session.ID()
			c.mu.Unlock()
			return nil
		case "wingman/status":
			body := map[string]any{"output": "", "breakpoints": []dap.SourceBreakpoint{}}
			if p.Source.Path != "" {
				path, err := c.sourcePath(p.Source.Path)
				if err != nil {
					return err
				}
				body["breakpoints"] = nonNilSourceBreakpoints(manager.Breakpoints(path))
			}
			if session := manager.ActiveSession(); session != nil {
				body["session"] = session.Status()
				body["output"] = session.Output()
				c.mu.Lock()
				c.bound = session.ID()
				c.mu.Unlock()
			}
			result = body
			return nil
		case "setBreakpoints":
			path, err := c.sourcePath(p.Source.Path)
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
			if resolved == nil {
				for _, point := range values {
					resolved = append(resolved, dap.Breakpoint{Line: point.Line, Column: point.Column, Verified: false, Message: "Pending debug session"})
				}
			}
			result = map[string]any{"breakpoints": nonNilDAPBreakpoints(resolved)}
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
		inspect := req.Command == "stackTrace" || req.Command == "scopes" || req.Command == "variables" || req.Command == "evaluate"
		if inspect && before.State != dap.StateStopped {
			return errors.New("debug session must be stopped to inspect it")
		}
		switch req.Command {
		case "continue", "next", "stepIn", "stepOut", "stepBack", "pause":
			methods := map[string]func(context.Context, int) error{"continue": session.Continue, "next": session.Next, "stepIn": session.StepIn, "stepOut": session.StepOut, "stepBack": session.StepBack, "pause": session.Pause}
			err = methods[req.Command](ctx, p.ThreadID)
			if req.Command == "continue" {
				result = map[string]any{"allThreadsContinued": true}
			}
		case "disconnect":
			if p.Terminate == nil || !*p.Terminate {
				result = map[string]any{}
				return nil
			}
			return manager.Stop(ctx, bound)
		case "threads":
			threads, callErr := session.Threads(ctx)
			err = callErr
			if threads == nil {
				threads = []dap.Thread{}
			}
			result = map[string]any{"threads": threads}
		case "stackTrace":
			if p.StartFrame < 0 || p.Levels < 0 || p.Levels > 1000 {
				return errors.New("invalid stack range")
			}
			if p.Levels == 0 {
				p.Levels = 100
			}
			frames, total, callErr := session.StackTrace(ctx, p.ThreadID, p.StartFrame, p.Levels)
			if callErr != nil {
				return callErr
			}
			if frames == nil {
				frames = []dap.StackFrame{}
			}
			for i := range frames {
				frames[i].ID, err = c.reference(before, frames[i].ID, "frame")
				if err != nil {
					return err
				}
			}
			result = map[string]any{"stackFrames": frames, "totalFrames": total}
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
			if p.Start < 0 || p.Count < 0 || p.Count > 500 {
				return errors.New("invalid variable range")
			}
			if p.Count == 0 {
				p.Count = 200
			}
			variables, err := session.Variables(ctx, id, p.Start, p.Count)
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
			if p.Context == "" {
				p.Context = "hover"
			}
			if p.Context != "hover" && p.Context != "watch" && p.Context != "repl" {
				return errors.New("unsupported evaluation context")
			}
			value, err := session.EvaluateContext(ctx, p.Expression, frameID, p.Context)
			if err != nil {
				return err
			}
			id, err := c.reference(before, value.VariablesReference, "variable")
			if err != nil {
				return err
			}
			result = map[string]any{"result": value.Result, "type": value.Type, "variablesReference": id, "namedVariables": value.NamedVariables, "indexedVariables": value.IndexedVariables}
		default:
			return fmt.Errorf("unsupported DAP command: %s", req.Command)
		}
		if inspect && debugStatusChanged(before, session.Status()) {
			return errDebugStateChanged
		}
		return err
	})
	return result, err
}

func (c *dapConnection) observe() {
	var previous *dap.Status
	output := ""
	for {
		var changed, stateChanged, outputChanged <-chan struct{}
		var status *dap.Status
		currentOutput := ""
		err := c.server.workspace.WithDAPManager(func(manager *dap.Manager) error {
			changed = manager.Changes()
			if session := manager.ActiveSession(); session != nil {
				stateChanged, outputChanged = session.Changes()
				current := session.Status()
				status = &current
				currentOutput = session.Output()
			}
			return nil
		})
		if err != nil {
			return
		}
		if status != nil && (previous == nil || debugStatusChanged(*previous, *status)) {
			c.event("wingman/status", map[string]any{"session": status})
			c.event("capabilities", map[string]any{"capabilities": map[string]any{"supportsStepBack": status.Capabilities.SupportsStepBack}})
			switch status.State {
			case dap.StateStopped:
				body := map[string]any{"reason": "pause"}
				if stop := status.Stop; stop != nil {
					body = map[string]any{"reason": stop.Reason, "description": stop.Description, "threadId": stop.ThreadID, "allThreadsStopped": stop.AllThreadsStopped, "hitBreakpointIds": stop.HitBreakpointIDs}
				}
				c.event("stopped", body)
			case dap.StateRunning:
				c.event("continued", map[string]any{"threadId": 0, "allThreadsContinued": true})
			case dap.StateTerminated:
				if status.ExitCode != nil {
					c.event("exited", map[string]any{"exitCode": *status.ExitCode})
				}
				c.event("terminated", map[string]any{})
			}
		} else if status == nil && previous != nil {
			c.event("wingman/status", map[string]any{"session": nil})
			c.event("terminated", map[string]any{})
		}
		if status != nil && (previous == nil || previous.SessionID != status.SessionID) {
			output = ""
		}
		if output != currentOutput {
			delta := strings.TrimPrefix(currentOutput, output)
			c.event("output", map[string]any{"category": "console", "output": delta, "_wingmanReplace": !strings.HasPrefix(currentOutput, output)})
			output = currentOutput
		}
		previous = status
		select {
		case <-c.ctx.Done():
			return
		case <-changed:
		case <-stateChanged:
		case <-outputChanged:
		}
	}
}
