package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

// askClient drives the approver's AskUserQuestion tiers with configurable
// support: form elicitation, permission selection, or hard failures.
type askClient struct {
	stubClient
	mu          sync.Mutex
	permCalls   int
	formCalls   int
	permErr     error
	permPick    string
	formErr     error
	formContent map[string]any
	formDecline bool
	formCancel  bool
	formSession string
}

func (c *askClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permCalls++
	c.mu.Unlock()
	if c.permErr != nil {
		return acp.RequestPermissionResponse{}, c.permErr
	}
	for _, o := range p.Options {
		if o.Name == c.permPick {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
				Selected: &acp.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
			}}, nil
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Cancelled: &acp.RequestPermissionOutcomeCancelled{},
	}}, nil
}

func (c *askClient) UnstableCreateElicitation(_ context.Context, req acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	c.formCalls++
	if req.Form != nil {
		c.formSession, _ = req.Form.Meta["sessionId"].(string)
	}
	c.mu.Unlock()
	switch {
	case c.formErr != nil:
		return acp.UnstableCreateElicitationResponse{}, c.formErr
	case c.formCancel:
		return acp.UnstableCreateElicitationResponse{Cancel: &acp.UnstableCreateElicitationCancel{Action: "cancel"}}, nil
	case c.formDecline:
		return acp.UnstableCreateElicitationResponse{Decline: &acp.UnstableCreateElicitationDecline{Action: "decline"}}, nil
	}
	return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{
		Action:  "accept",
		Content: c.formContent,
	}}, nil
}

func (c *askClient) UnstableCompleteElicitation(context.Context, acp.UnstableCompleteElicitationNotification) error {
	return nil
}

func (c *askClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	return nil
}

func (c *askClient) calls() (perm, form int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.permCalls, c.formCalls
}

type askResponse struct {
	Behavior string          `json:"behavior"`
	Message  string          `json:"message"`
	Input    json.RawMessage `json:"updatedInput"`
}

func runAskUserQuestion(t *testing.T, client acp.Client, askForm bool) (askResponse, *askClient) {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close(); _ = clientSide.Close() })
	conn := acp.NewAgentSideConnection(New(Options{}), agentSide, agentSide)
	_ = acp.NewClientSideConnection(client, clientSide, clientSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	app := &approver{ctx: ctx, conn: conn, sid: "test", out: &streamWriter{w: &out}, askForm: askForm}

	var req controlRequest
	req.RequestID = "r1"
	req.Request.Subtype = "can_use_tool"
	req.Request.ToolName = "AskUserQuestion"
	req.Request.ToolUseID = "tu1"
	req.Request.Input = json.RawMessage(`{"questions":[{"question":"Which color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]}]}`)
	app.handle(req)

	var env struct {
		Response struct {
			Response askResponse `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("parse control response %q: %v", out.String(), err)
	}
	ac, _ := client.(*askClient)
	return env.Response.Response, ac
}

func askAnswerOf(t *testing.T, resp askResponse) map[string]any {
	t.Helper()
	var input struct {
		Answers map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(resp.Input, &input); err != nil {
		t.Fatalf("parse updatedInput %s: %v", resp.Input, err)
	}
	return input.Answers
}

func TestAskUserQuestionFormTier(t *testing.T) {
	client := &askClient{formContent: map[string]any{"question_0": "Blue"}}
	resp, c := runAskUserQuestion(t, client, true)
	if resp.Behavior != "allow" {
		t.Fatalf("behavior = %q (%s)", resp.Behavior, resp.Message)
	}
	if answers := askAnswerOf(t, resp); answers["Which color?"] != "Blue" {
		t.Errorf("answers = %#v", answers)
	}
	if perm, form := c.calls(); form != 1 || perm != 0 {
		t.Errorf("calls: form=%d perm=%d, want form only", form, perm)
	}
	if c.formSession != "test" {
		t.Errorf("elicitation session metadata = %q, want test", c.formSession)
	}
}

func TestAskUserQuestionFormDecline(t *testing.T) {
	client := &askClient{formDecline: true}
	resp, _ := runAskUserQuestion(t, client, true)
	if resp.Behavior != "allow" {
		t.Fatalf("behavior = %q", resp.Behavior)
	}
	if answers := askAnswerOf(t, resp); len(answers) != 0 {
		t.Errorf("declined form should yield empty answers, got %#v", answers)
	}
}

func TestAskUserQuestionFormCancel(t *testing.T) {
	client := &askClient{formCancel: true}
	resp, _ := runAskUserQuestion(t, client, true)
	if resp.Behavior != "deny" || !strings.Contains(resp.Message, "cancelled") {
		t.Fatalf("cancel should deny, got %q (%s)", resp.Behavior, resp.Message)
	}
}

func TestAskUserQuestionFormErrorFallsBackToPermissions(t *testing.T) {
	client := &askClient{formErr: errors.New("method not supported"), permPick: "Blue"}
	resp, c := runAskUserQuestion(t, client, true)
	if resp.Behavior != "allow" {
		t.Fatalf("behavior = %q (%s)", resp.Behavior, resp.Message)
	}
	if answers := askAnswerOf(t, resp); answers["Which color?"] != "Blue" {
		t.Errorf("answers = %#v", answers)
	}
	if perm, form := c.calls(); form != 1 || perm != 1 {
		t.Errorf("calls: form=%d perm=%d, want fallback after form error", form, perm)
	}
}

func TestAskUserQuestionPermissionTier(t *testing.T) {
	client := &askClient{permPick: "Red"}
	resp, c := runAskUserQuestion(t, client, false)
	if resp.Behavior != "allow" {
		t.Fatalf("behavior = %q", resp.Behavior)
	}
	if answers := askAnswerOf(t, resp); answers["Which color?"] != "Red" {
		t.Errorf("answers = %#v", answers)
	}
	if perm, form := c.calls(); form != 0 || perm != 1 {
		t.Errorf("calls: form=%d perm=%d, want permission only", form, perm)
	}
}

func TestAskUserQuestionPermissionSkip(t *testing.T) {
	client := &askClient{permPick: "Skip"}
	resp, _ := runAskUserQuestion(t, client, false)
	if resp.Behavior != "allow" {
		t.Fatalf("behavior = %q", resp.Behavior)
	}
	if answers := askAnswerOf(t, resp); len(answers) != 0 {
		t.Errorf("skip should yield empty answers, got %#v", answers)
	}
}

func TestAskUserQuestionUnsupportedClientDenies(t *testing.T) {
	client := &askClient{formErr: errors.New("no form"), permErr: errors.New("no permissions")}
	resp, _ := runAskUserQuestion(t, client, true)
	if resp.Behavior != "deny" || !strings.Contains(resp.Message, "Could not present") {
		t.Fatalf("unsupported client should deny with reason, got %q (%s)", resp.Behavior, resp.Message)
	}
}

func TestAskUserQuestionInvalidInputDenies(t *testing.T) {
	agentSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close(); _ = clientSide.Close() })
	conn := acp.NewAgentSideConnection(New(Options{}), agentSide, agentSide)
	_ = acp.NewClientSideConnection(&askClient{}, clientSide, clientSide)

	var out bytes.Buffer
	app := &approver{ctx: context.Background(), conn: conn, sid: "test", out: &streamWriter{w: &out}}
	var req controlRequest
	req.RequestID = "r1"
	req.Request.Subtype = "can_use_tool"
	req.Request.ToolName = "AskUserQuestion"
	req.Request.Input = json.RawMessage(`{"questions":[]}`)
	app.handle(req)
	if !strings.Contains(out.String(), `"deny"`) || !strings.Contains(out.String(), "no valid questions") {
		t.Fatalf("invalid input should deny, got %s", out.String())
	}
}

func TestToolCallTrackerCompletesAndDropsFailedStarts(t *testing.T) {
	tracker := newToolCallTracker()
	starts, refinements := 0, 0
	if err := tracker.emit("tool-1", func() error { starts++; return nil }, func() error { refinements++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := tracker.emit("tool-1", func() error { starts++; return nil }, func() error { refinements++; return nil }); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || refinements != 1 || !tracker.has("tool-1") {
		t.Fatalf("starts=%d refinements=%d active=%v", starts, refinements, tracker.has("tool-1"))
	}
	if !tracker.complete("tool-1") || tracker.has("tool-1") || tracker.complete("tool-1") {
		t.Fatal("completed tool call should be removed exactly once")
	}

	sentinel := errors.New("send failed")
	if err := tracker.emit("tool-2", func() error { return sentinel }, func() error { return nil }); !errors.Is(err, sentinel) {
		t.Fatalf("start error = %v", err)
	}
	if tracker.has("tool-2") {
		t.Fatal("failed tool-call start must not remain active")
	}
}

func TestPermissionMetadataForAlwaysAllow(t *testing.T) {
	req := controlRequestBody{
		ToolName:              "Bash",
		PermissionSuggestions: json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm *"}],"behavior":"allow","destination":"projectSettings"}]`),
	}
	meta := permissionMetadataForAlwaysAllow(req)
	changes, _ := meta["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("metadata = %#v", meta)
	}
	change := changes[0].(map[string]any)
	if change["description"] != "Allow Bash calls matching npm *" {
		t.Fatalf("description = %q", change["description"])
	}
	lifetime := change["lifetime"].(map[string]any)
	if lifetime["scope"] != "persistent" || lifetime["storage"] != "project" {
		t.Fatalf("lifetime = %#v", lifetime)
	}
}

type recordingPermissionClient struct {
	stubClient
	pick acp.PermissionOptionId
	got  acp.RequestPermissionRequest
}

func (*recordingPermissionClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	return nil
}

func (c *recordingPermissionClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.got = p
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: c.pick},
	}}, nil
}

type permissionReply struct {
	Behavior    string           `json:"behavior"`
	Permissions []map[string]any `json:"updatedPermissions"`
}

func runPermission(t *testing.T, client *recordingPermissionClient, app *approver, body string) permissionReply {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = agentSide.Close(); _ = clientSide.Close() })
	app.conn = acp.NewAgentSideConnection(New(Options{}), agentSide, agentSide)
	_ = acp.NewClientSideConnection(client, clientSide, clientSide)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	app.ctx, app.sid, app.out = ctx, "test", &streamWriter{w: &out}
	var req controlRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	app.handle(req)
	var env struct {
		Response struct {
			Response permissionReply `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("parse control response %q: %v", out.String(), err)
	}
	return env.Response.Response
}

func optionIDs(options []acp.PermissionOption) []acp.PermissionOptionId {
	ids := make([]acp.PermissionOptionId, len(options))
	for i, o := range options {
		ids[i] = o.OptionId
	}
	return ids
}

func TestPermissionOptionsFollowCLIHints(t *testing.T) {
	for _, tt := range []struct {
		name  string
		extra string
		want  []acp.PermissionOptionId
	}{
		{"plain", ``, []acp.PermissionOptionId{optionAllowOnce, optionAllowAlways, optionRejectOnce}},
		{"default to no", `,"default_to_no":true`, []acp.PermissionOptionId{optionRejectOnce, optionAllowOnce, optionAllowAlways}},
		{"suppressed rule", `,"suppress_always_allow_rule":true`, []acp.PermissionOptionId{optionAllowOnce, optionRejectOnce}},
		{"matched ask rule", `,"matched_ask_rule":{"toolName":"Bash"}`, []acp.PermissionOptionId{optionAllowOnce, optionRejectOnce}},
		{"safety ask", `,"default_to_no":true,"suppress_always_allow_rule":true`, []acp.PermissionOptionId{optionRejectOnce, optionAllowOnce}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &recordingPermissionClient{pick: optionAllowOnce}
			reply := runPermission(t, client, &approver{}, `{"request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"tu1","input":{"command":"rm -rf build"}`+tt.extra+`}}`)
			if got := optionIDs(client.got.Options); !slices.Equal(got, tt.want) {
				t.Fatalf("options = %v, want %v", got, tt.want)
			}
			if reply.Behavior != "allow" || reply.Permissions != nil {
				t.Fatalf("reply = %+v", reply)
			}
		})
	}
}

func TestPermissionRequestCarriesMCPServer(t *testing.T) {
	client := &recordingPermissionClient{pick: optionRejectOnce}
	reply := runPermission(t, client, &approver{}, `{"request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"mcp__github__list_issues","tool_use_id":"tu1","input":{},"mcp_server":{"name":"github","source":"sdk"}}}`)
	claudeMeta, _ := client.got.ToolCall.Meta["claudeCode"].(map[string]any)
	server, _ := claudeMeta["mcpServer"].(map[string]any)
	if server["name"] != "github" || server["source"] != "sdk" || claudeMeta["toolName"] != "mcp__github__list_issues" {
		t.Fatalf("tool call meta = %#v", client.got.ToolCall.Meta)
	}
	if reply.Behavior != "deny" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestExitPlanModeOffersElevatedModes(t *testing.T) {
	const exitPlan = `{"request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","tool_use_id":"tu1","input":{"plan":"do it"}}}`
	for _, tt := range []struct {
		name        string
		allowBypass bool
		prePlan     string
		pick        acp.PermissionOptionId
		wantOptions []acp.PermissionOptionId
		wantMode    string
	}{
		{"auto leads", true, "agent", optionExitPlanAuto, []acp.PermissionOptionId{optionExitPlanAuto, optionExitPlanBypass, optionExitPlanReject}, "agent"},
		{"bypass leads after bypass", true, "unattended", optionExitPlanBypass, []acp.PermissionOptionId{optionExitPlanBypass, optionExitPlanAuto, optionExitPlanReject}, "unattended"},
		{"bypass unavailable", false, "unattended", optionExitPlanAuto, []acp.PermissionOptionId{optionExitPlanAuto, optionExitPlanReject}, "agent"},
		{"keep planning", true, "", optionExitPlanReject, []acp.PermissionOptionId{optionExitPlanAuto, optionExitPlanBypass, optionExitPlanReject}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &recordingPermissionClient{pick: tt.pick}
			var applied string
			app := &approver{allowBypass: tt.allowBypass, prePlanMode: func() string { return tt.prePlan }, applyMode: func(id string) { applied = id }}
			reply := runPermission(t, client, app, exitPlan)
			if got := optionIDs(client.got.Options); !slices.Equal(got, tt.wantOptions) {
				t.Fatalf("options = %v, want %v", got, tt.wantOptions)
			}
			if applied != tt.wantMode {
				t.Fatalf("applied mode = %q, want %q", applied, tt.wantMode)
			}
			if tt.wantMode == "" {
				if reply.Behavior != "deny" {
					t.Fatalf("reply = %+v", reply)
				}
				return
			}
			if reply.Behavior != "allow" || len(reply.Permissions) != 1 || reply.Permissions[0]["mode"] != findMode(tt.wantMode).permissionMode {
				t.Fatalf("reply = %+v", reply)
			}
		})
	}
}

func TestAskUserQuestionFormKeepsPickAndNotes(t *testing.T) {
	client := &askClient{formContent: map[string]any{"question_0": "Blue", "question_0_custom": "navy if possible"}}
	resp, _ := runAskUserQuestion(t, client, true)
	var input struct {
		Answers     map[string]any `json:"answers"`
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(resp.Input, &input); err != nil {
		t.Fatal(err)
	}
	notes, _ := input.Annotations["Which color?"].(map[string]any)
	if input.Answers["Which color?"] != "Blue" || notes["notes"] != "navy if possible" {
		t.Fatalf("updatedInput = %s", resp.Input)
	}
}
