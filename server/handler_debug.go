package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/adrianliechti/wingman-agent/pkg/debugadapter"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
)

const debugRequestLimit = 2 << 20

var errDebugStateChanged = errors.New("debug session state changed; refresh and try again")

type debugPlanBreakpoint struct {
	FilePath     string `json:"file_path"`
	Line         int    `json:"line"`
	Column       int    `json:"column,omitempty"`
	Condition    string `json:"condition,omitempty"`
	HitCondition string `json:"hit_condition,omitempty"`
	LogMessage   string `json:"log_message,omitempty"`
}

type debugLaunchPlan struct {
	ID                  string                `json:"id,omitempty"`
	Action              string                `json:"action"`
	Title               string                `json:"title"`
	Adapter             string                `json:"adapter"`
	TerminalAvailable   bool                  `json:"terminal_available"`
	ProjectDir          string                `json:"project_dir"`
	Request             string                `json:"request"`
	IO                  string                `json:"io"`
	Configuration       map[string]any        `json:"configuration"`
	Breakpoints         []debugPlanBreakpoint `json:"breakpoints"`
	FunctionBreakpoints []string              `json:"function_breakpoints"`
	PreLaunch           *dap.ProcessLaunch    `json:"prelaunch,omitempty"`
}

func (s *Server) handleDebugTargets(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string  `json:"path"`
		Content *string `json:"content,omitempty"`
	}
	if err := decodeDebugJSON(w, r, &body); err != nil {
		return
	}
	rel, ok := s.resolveExistingRegularFile(w, body.Path)
	if !ok {
		return
	}
	var source []byte
	if body.Content != nil {
		source = []byte(*body.Content)
	} else {
		var err error
		source, err = s.workspace.Root.ReadFile(rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	targets, err := s.workspace.DebugRegistry().DetectFile(filepath.ToSlash(rel), source)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, map[string]any{"targets": nonNilDebugTargets(targets)})
}

type debugPlanRequest struct {
	Action      string `json:"action"`
	TargetID    string `json:"target_id"`
	CurrentPath string `json:"current_path"`
	Install     bool   `json:"install,omitempty"`
}

type debugPlanEvent struct {
	Type     string                `json:"type"`
	Progress *devtools.Progress    `json:"progress,omitempty"`
	Plan     *debugLaunchPlan      `json:"plan,omitempty"`
	Error    string                `json:"error,omitempty"`
	Tools    []devtools.ToolStatus `json:"tools,omitempty"`
	Warning  string                `json:"warning,omitempty"`
}

func (s *Server) handleDebugPlan(w http.ResponseWriter, r *http.Request) {
	var request debugPlanRequest
	if err := decodeDebugJSON(w, r, &request); err != nil {
		return
	}
	request.Action = strings.ToLower(strings.TrimSpace(request.Action))
	if request.Action == "" {
		request.Action = "debug"
	}
	if request.Action != "run" && request.Action != "debug" {
		http.Error(w, "action must be run or debug", http.StatusBadRequest)
		return
	}
	request.TargetID = strings.TrimSpace(request.TargetID)
	request.CurrentPath = strings.TrimSpace(request.CurrentPath)
	if request.TargetID == "" || request.CurrentPath == "" {
		http.Error(w, "target_id and current_path are required", http.StatusBadRequest)
		return
	}
	currentFile, ok := s.resolveExistingRegularFile(w, request.CurrentPath)
	if !ok {
		return
	}
	targets, err := s.detectDebugTargets(currentFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var selected *debugadapter.Target
	for i := range targets {
		if targets[i].ID == request.TargetID {
			candidate := targets[i]
			selected = &candidate
			break
		}
	}
	if selected == nil {
		http.Error(w, "debug target is no longer available", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if s.ctx != nil {
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
	}
	streaming := strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	emit := func(event debugPlanEvent) {
		if !streaming {
			writeJSON(w, event)
			return
		}
		if err := encoder.Encode(event); err != nil {
			cancel()
			return
		}
		if err := controller.Flush(); err != nil {
			cancel()
		}
	}
	fail := func(err error, status int) {
		if streaming {
			emit(debugPlanEvent{Type: "error", Error: err.Error()})
		} else {
			http.Error(w, err.Error(), status)
		}
	}
	if streaming {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		if err := controller.Flush(); err != nil {
			return
		}
	}
	statuses, err := s.workspace.DebugToolStatus(ctx, selected.Language)
	if err != nil {
		fail(err, http.StatusServiceUnavailable)
		return
	}
	missing := slices.ContainsFunc(statuses, func(status devtools.ToolStatus) bool { return !status.Installed })
	if missing && !request.Install {
		emit(debugPlanEvent{Type: "installation_required", Tools: statuses})
		return
	}
	if streaming {
		emit(debugPlanEvent{Type: "tools", Tools: statuses})
	}
	if slices.ContainsFunc(statuses, func(status devtools.ToolStatus) bool { return !status.Installed && !status.Installable }) {
		fail(errors.New("managed debugger installation is disabled or unavailable; enable managed tool installation and retry"), http.StatusServiceUnavailable)
		return
	}
	previousTool := ""
	changed, installErr := s.workspace.UpdateDebugTools(ctx, selected.Language, request.Install, func(progress devtools.Progress) {
		if streaming {
			// A completed dependency should show its installed state while
			// setup continues with the next tool (for example Java's host).
			if progress.Phase == devtools.ProgressChecking && previousTool != "" {
				if snapshot, err := s.workspace.DebugToolStatus(ctx, selected.Language); err == nil {
					emit(debugPlanEvent{Type: "tools", Tools: snapshot})
				}
			}
			previousTool = progress.Tool
			emit(debugPlanEvent{Type: "progress", Progress: &progress})
		}
	})
	if changed {
		s.broadcast(Frame{Type: EvtWorkspaceChanged})
	}
	if ctx.Err() != nil {
		return
	}
	if errors.Is(installErr, dap.ErrActiveSession) || errors.Is(installErr, dap.ErrBusy) {
		fail(installErr, http.StatusConflict)
		return
	}
	statuses, err = s.workspace.DebugToolStatus(ctx, selected.Language)
	if err != nil {
		fail(err, http.StatusServiceUnavailable)
		return
	}
	missing = slices.ContainsFunc(statuses, func(status devtools.ToolStatus) bool { return !status.Installed })
	if missing && !request.Install {
		emit(debugPlanEvent{Type: "installation_required", Tools: statuses})
		return
	}
	if streaming {
		emit(debugPlanEvent{Type: "tools", Tools: statuses})
	}
	if missing {
		if installErr == nil {
			installErr = errors.New("required debugger tools are unavailable; retry installation")
		}
		fail(installErr, http.StatusServiceUnavailable)
		return
	}
	var adapterInfo []dap.AdapterInfo
	err = s.workspace.WithDAPManager(func(manager *dap.Manager) error {
		values, err := manager.Adapters(ctx)
		adapterInfo = values
		return err
	})
	if err != nil {
		fail(errors.Join(err, installErr), http.StatusServiceUnavailable)
		return
	}
	adapterInfo, err = selectTargetDebugAdapter(adapterInfo, *selected)
	if err != nil {
		fail(errors.Join(err, installErr), http.StatusBadRequest)
		return
	}
	projectDir, err := selectTargetDebugProject(s.workspace.RootPath, adapterInfo[0], *selected)
	if err != nil {
		fail(err, http.StatusBadRequest)
		return
	}
	profile, err := s.workspace.DebugRegistry().Plan(adapterInfo[0].Language, debugadapter.Request{
		Action: request.Action, WorkspaceDir: s.workspace.RootPath, ProjectDir: projectDir,
		Target: *selected,
	})
	if err != nil {
		fail(err, http.StatusUnprocessableEntity)
		return
	}
	plan := debugLaunchPlan{
		Action: request.Action, Title: profile.Title,
		Adapter: adapterInfo[0].Name, ProjectDir: profile.ProjectDir, Request: profile.Request,
		TerminalAvailable: profile.SupportsTerminal && adapterInfo[0].TerminalStrategy != dap.TerminalUnsupported && terminal.Supported(),
		IO:                string(profile.IO), Configuration: profile.Configuration,
		FunctionBreakpoints: profile.FunctionBreakpoints,
		PreLaunch:           profile.PreLaunch,
	}
	for _, breakpoint := range profile.Breakpoints {
		plan.Breakpoints = append(plan.Breakpoints, debugPlanBreakpoint{
			FilePath: breakpoint.FilePath, Line: breakpoint.Line, Column: breakpoint.Column,
		})
	}
	if err := s.validateDebugPlan(&plan, adapterInfo); err != nil {
		fail(fmt.Errorf("invalid deterministic debug configuration: %w", err), http.StatusUnprocessableEntity)
		return
	}
	if err := s.workspace.WithDAPManager(func(manager *dap.Manager) error {
		plan.ID = manager.RememberPlan(cloneDebugLaunchPlan(plan))
		return nil
	}); err != nil {
		fail(err, http.StatusServiceUnavailable)
		return
	}
	if streaming {
		warning := ""
		if installErr != nil {
			warning = "Could not update debugger tools. Using the installed version.\n" + installErr.Error()
		}
		emit(debugPlanEvent{Type: "plan", Plan: &plan, Warning: warning})
	} else {
		writeJSON(w, plan)
	}
}

func decodeDebugJSON(w http.ResponseWriter, r *http.Request, target any) error {
	return decodeJSONRequest(w, r, target, debugRequestLimit)
}

func (s *Server) detectDebugTargets(currentFile string) ([]debugadapter.Target, error) {
	source, err := s.workspace.Root.ReadFile(currentFile)
	if err != nil {
		return nil, err
	}
	return s.workspace.DebugRegistry().DetectFile(filepath.ToSlash(currentFile), source)
}

func selectTargetDebugAdapter(values []dap.AdapterInfo, target debugadapter.Target) ([]dap.AdapterInfo, error) {
	matching := make([]dap.AdapterInfo, 0, 1)
	for _, adapter := range values {
		if strings.EqualFold(adapter.Language, target.Language) {
			matching = append(matching, adapter)
		}
	}
	if len(matching) == 0 {
		return nil, fmt.Errorf("no installed debug adapter supports %s", target.Language)
	}
	if len(matching) > 1 {
		return nil, fmt.Errorf("multiple %s debug adapters are available; choose one explicitly", target.Language)
	}
	return matching, nil
}

func selectTargetDebugProject(root string, adapter dap.AdapterInfo, target debugadapter.Target) (string, error) {
	targetDir := filepath.Join(root, filepath.FromSlash(target.Directory))
	best := ""
	for _, project := range adapter.Projects {
		rel, err := filepath.Rel(project, targetDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(project) > len(best) {
			best = project
		}
	}
	if best == "" {
		return "", fmt.Errorf("target %s is not inside a detected %s project", target.Path, adapter.Language)
	}
	rel, err := filepath.Rel(root, best)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("detected debug project is outside the workspace")
	}
	if rel == "." {
		return ".", nil
	}
	return filepath.ToSlash(rel), nil
}

func nonNilDebugTargets(values []debugadapter.Target) []debugadapter.Target {
	if values == nil {
		return []debugadapter.Target{}
	}
	return values
}

func nonNilSourceBreakpoints(values []dap.SourceBreakpoint) []dap.SourceBreakpoint {
	if values == nil {
		return []dap.SourceBreakpoint{}
	}
	return values
}

func nonNilDAPBreakpoints(values []dap.Breakpoint) []dap.Breakpoint {
	if values == nil {
		return []dap.Breakpoint{}
	}
	return values
}

func debugStatusChanged(before, after dap.Status) bool {
	return before.SessionID != after.SessionID || before.StateVersion != after.StateVersion
}

func (s *Server) debugStartOptions(plan debugLaunchPlan) dap.StartOptions {
	options := dap.StartOptions{
		Adapter:       plan.Adapter,
		ProjectDir:    plan.ProjectDir,
		Request:       plan.Request,
		IO:            dap.IOMode(plan.IO),
		Configuration: cloneJSONMap(plan.Configuration),
		PreLaunch:     cloneDebugProcessLaunch(plan.PreLaunch),
		Breakpoints:   make(map[string][]dap.SourceBreakpoint),
	}
	for _, breakpoint := range plan.Breakpoints {
		path := filepath.Join(s.workspace.RootPath, filepath.FromSlash(breakpoint.FilePath))
		options.Breakpoints[path] = append(options.Breakpoints[path], dap.SourceBreakpoint{
			Line: breakpoint.Line, Column: breakpoint.Column, Condition: breakpoint.Condition,
			HitCondition: breakpoint.HitCondition, LogMessage: breakpoint.LogMessage,
		})
	}
	for _, name := range plan.FunctionBreakpoints {
		if name = strings.TrimSpace(name); name != "" {
			options.FunctionBreakpoints = append(options.FunctionBreakpoints, dap.FunctionBreakpoint{Name: name})
		}
	}
	return options
}

func cloneDebugLaunchPlan(plan debugLaunchPlan) debugLaunchPlan {
	plan.Configuration = cloneJSONMap(plan.Configuration)
	plan.Breakpoints = slices.Clone(plan.Breakpoints)
	plan.FunctionBreakpoints = slices.Clone(plan.FunctionBreakpoints)
	plan.PreLaunch = cloneDebugProcessLaunch(plan.PreLaunch)
	return plan
}

// reviewedDebugPlan starts from the plan the host generated. The browser may
// change only what its review dialog offers: I/O, adapter options and whether
// the plan's initial breakpoints apply. Commands such as prelaunch never come
// from the client.
func (s *Server) reviewedDebugPlan(manager *dap.Manager, id string, edits *debugLaunchPlan) (debugLaunchPlan, error) {
	value, ok := manager.ReviewedPlan(id)
	stored, valid := value.(debugLaunchPlan)
	if !ok || !valid {
		return debugLaunchPlan{}, errors.New("the reviewed debug plan has expired; prepare the launch again")
	}
	plan := cloneDebugLaunchPlan(stored)
	if edits == nil {
		return plan, nil
	}
	if edits.IO != "" {
		plan.IO = edits.IO
	}
	if edits.Configuration != nil {
		plan.Configuration = cloneJSONMap(edits.Configuration)
	}
	for _, breakpoint := range edits.Breakpoints {
		if !slices.Contains(stored.Breakpoints, breakpoint) {
			return debugLaunchPlan{}, errors.New("launch breakpoints must come from the reviewed plan")
		}
	}
	for _, name := range edits.FunctionBreakpoints {
		if !slices.Contains(stored.FunctionBreakpoints, name) {
			return debugLaunchPlan{}, errors.New("launch function breakpoints must come from the reviewed plan")
		}
	}
	plan.Breakpoints = slices.Clone(edits.Breakpoints)
	plan.FunctionBreakpoints = slices.Clone(edits.FunctionBreakpoints)
	return plan, nil
}

func cloneDebugProcessLaunch(value *dap.ProcessLaunch) *dap.ProcessLaunch {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Args = slices.Clone(value.Args)
	return &clone
}

func cloneJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return maps.Clone(value)
}

func (s *Server) validateDebugPlan(plan *debugLaunchPlan, adapters []dap.AdapterInfo) error {
	plan.Action = strings.ToLower(strings.TrimSpace(plan.Action))
	if plan.Action == "" {
		plan.Action = "debug"
	}
	if plan.Action != "run" && plan.Action != "debug" {
		return errors.New("action must be run or debug")
	}
	var selectedAdapter *dap.AdapterInfo
	for index := range adapters {
		adapter := &adapters[index]
		if strings.EqualFold(adapter.Name, plan.Adapter) {
			plan.Adapter = adapter.Name
			selectedAdapter = adapter
			break
		}
	}
	if selectedAdapter == nil {
		var matching []int
		for index := range adapters {
			if strings.EqualFold(adapters[index].Language, plan.Adapter) {
				matching = append(matching, index)
			}
		}
		if len(matching) > 1 {
			return fmt.Errorf("multiple %s debug adapters are available; choose one by name", plan.Adapter)
		}
		if len(matching) == 1 {
			selectedAdapter = &adapters[matching[0]]
			plan.Adapter = selectedAdapter.Name
		}
	}
	if selectedAdapter == nil {
		return fmt.Errorf("adapter %q is not available", plan.Adapter)
	}
	plan.IO = strings.TrimSpace(plan.IO)
	if plan.IO == "" {
		plan.IO = string(dap.IOOutput)
	}
	if plan.IO != string(dap.IOOutput) && plan.IO != string(dap.IOTerminal) {
		return fmt.Errorf("I/O mode must be %s or %s", dap.IOOutput, dap.IOTerminal)
	}
	if plan.IO == string(dap.IOTerminal) && (selectedAdapter.TerminalStrategy == dap.TerminalUnsupported || !terminal.Supported()) {
		return fmt.Errorf("adapter %s cannot use a terminal on this host", selectedAdapter.Name)
	}
	plan.Request = strings.ToLower(strings.TrimSpace(plan.Request))
	if plan.Request != "launch" && plan.Request != "attach" {
		return errors.New("request must be launch or attach")
	}
	if plan.Configuration == nil {
		plan.Configuration = map[string]any{}
	}
	if plan.PreLaunch != nil {
		plan.PreLaunch.Title = strings.TrimSpace(plan.PreLaunch.Title)
		plan.PreLaunch.Command = strings.TrimSpace(plan.PreLaunch.Command)
		if plan.PreLaunch.Title == "" || plan.PreLaunch.Command == "" || !filepath.IsAbs(plan.PreLaunch.Command) {
			return errors.New("prelaunch process requires a title and absolute command")
		}
		info, err := os.Stat(plan.PreLaunch.Command)
		if err != nil || info.IsDir() {
			return fmt.Errorf("prelaunch command %q is unavailable", plan.PreLaunch.Command)
		}
		if plan.PreLaunch.ReadyURL != "" {
			readyURL, err := url.Parse(plan.PreLaunch.ReadyURL)
			if err != nil || (readyURL.Scheme != "http" && readyURL.Scheme != "https") || !loopbackDebugHost(readyURL.Hostname()) {
				return errors.New("prelaunch ready_url must be an HTTP URL on this machine")
			}
			plan.PreLaunch.ReadyURL = readyURL.String()
		}
	}
	projectPath, err := dap.ResolveWorkspaceDirectory(s.workspace.RootPath, plan.ProjectDir)
	if err != nil {
		return err
	}
	project, err := filepath.Rel(s.workspace.RootPath, projectPath)
	if err != nil {
		return errors.New("project_dir must be inside the workspace")
	}
	if project == "." {
		plan.ProjectDir = "."
	} else {
		plan.ProjectDir = filepath.ToSlash(project)
	}
	for index := range plan.Breakpoints {
		breakpoint := &plan.Breakpoints[index]
		if breakpoint.Line < 1 || breakpoint.Column < 0 {
			return fmt.Errorf("breakpoint %d has an invalid position", index+1)
		}
		rel, ok := s.workspaceRel(breakpoint.FilePath)
		if !ok {
			return fmt.Errorf("breakpoint %d has an invalid path", index+1)
		}
		info, err := s.workspace.Root.Stat(rel)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("breakpoint path %q does not exist", breakpoint.FilePath)
		}
		breakpoint.FilePath = filepath.ToSlash(rel)
	}
	if _, err := dap.ResolveConfigurationPaths(s.workspace.RootPath, projectPath, selectedAdapter.ConfigurationPaths, plan.Configuration); err != nil {
		return err
	}
	plan.Title = strings.TrimSpace(plan.Title)
	if plan.Title == "" {
		plan.Title = "Debug session"
	}
	if plan.Breakpoints == nil {
		plan.Breakpoints = []debugPlanBreakpoint{}
	}
	if plan.FunctionBreakpoints == nil {
		plan.FunctionBreakpoints = []string{}
	}
	return nil
}

func loopbackDebugHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
