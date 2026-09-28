package agent

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
)

func TestConfirmWithoutUIFailsClosed(t *testing.T) {
	a := &Agent{}
	allowed, err := a.confirm(context.Background(), "run dangerous command?")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("missing UI approved a confirmation")
	}
}

func TestEffortReturnsIndependentValues(t *testing.T) {
	a := &Agent{}
	_, values := a.Effort("")
	values[0] = "changed"
	_, again := a.Effort("")
	if again[0] != "auto" {
		t.Fatalf("caller mutated shared effort values: %v", again)
	}
}

func TestClosedAgentRejectsNewSession(t *testing.T) {
	a := &Agent{closed: true}
	if _, err := a.NewSession(context.Background()); err == nil {
		t.Fatal("closed agent created a session")
	}
}

func TestSessionFileRootsKeepSystemSkillsReadOnly(t *testing.T) {
	t.Setenv("WINGMAN_SANDBOX", "")
	volumeRoot := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	systemRoot := filepath.Join(volumeRoot, "wingman-system-skills", ".system")
	ws := &code.Workspace{
		RootPath:         t.TempDir(),
		ScratchPath:      t.TempDir(),
		SystemSkillsPath: systemRoot,
	}

	readRoots, writeRoots := sessionFileRoots(ws)
	if !pathCoveredByRoots(systemRoot, readRoots) {
		t.Fatalf("system skills root %q is not readable from %#v", systemRoot, readRoots)
	}
	if pathCoveredByRoots(systemRoot, writeRoots) {
		t.Fatalf("system skills root %q is writable from %#v", systemRoot, writeRoots)
	}
}

func pathCoveredByRoots(path string, roots []string) bool {
	for _, root := range roots {
		if root == "*" {
			return true
		}
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func TestUnattendedApprovesAndResolvesPromptsWithoutUI(t *testing.T) {
	s := &sessionState{}
	s.setMode(modeUnattended)
	a := &Agent{sessions: map[string]*sessionState{"s1": s}}
	ctx := code.WithSessionID(context.Background(), "s1")

	allowed, err := a.confirm(ctx, "edit a file?")
	if err != nil || !allowed {
		t.Fatalf("unattended confirmation = %v, %v", allowed, err)
	}
	res, err := a.elicit(ctx, tool.ElicitRequest{Fields: []tool.ElicitField{{
		Name: "choice", Required: true, Enum: []string{"Recommended", "Alternative"},
	}}})
	if err != nil || res.Action != tool.ElicitAccept || res.Content["choice"] != "Recommended" {
		t.Fatalf("unattended elicitation = %#v, %v", res, err)
	}
	res, err = a.elicit(ctx, tool.ElicitRequest{Fields: []tool.ElicitField{{
		Name: "detail", Required: true, Type: "string",
	}}})
	if err != nil || res.Action != tool.ElicitDecline {
		t.Fatalf("required free-text elicitation = %#v, %v", res, err)
	}
}

func TestModeSwitchReachesToolsWhileCatalogStaysPinned(t *testing.T) {
	s := &sessionState{
		parent: &Agent{workspace: &code.Workspace{}},
		toolSet: tool.NewSet(
			tool.Tool{Name: "elicit"},
			tool.Tool{Name: "read"},
		),
	}
	s.setMode(modeAgent)
	s.turnTools.Store([]tool.Tool{{Name: "mcp_search"}})

	has := func(name string) bool {
		return slices.ContainsFunc(s.tools(), func(t tool.Tool) bool { return t.Name == name })
	}

	if !has("elicit") || !has("mcp_search") {
		t.Fatalf("agent mode tools = %#v", s.tools())
	}

	s.setMode(modeUnattended)
	if has("elicit") {
		t.Fatalf("mode switch did not reach the tool set mid-turn: %#v", s.tools())
	}
	if !has("mcp_search") {
		t.Fatalf("pinned catalog lost mid-turn: %#v", s.tools())
	}

	s.clearCancel(s.cancelGen)
	if has("mcp_search") {
		t.Fatalf("catalog stayed pinned after the turn: %#v", s.tools())
	}
}

func TestUnattendedModeOwnsToolsAndInstructions(t *testing.T) {
	s := &sessionState{
		parent: &Agent{workspace: &code.Workspace{}},
		toolSet: tool.NewSet(
			tool.Tool{Name: "elicit"},
			tool.Tool{Name: "read"},
		),
	}
	s.setMode(modeUnattended)

	tools := s.tools()
	if len(tools) != 1 || tools[0].Name != "read" {
		t.Fatalf("unattended tools = %#v, want read without elicit", tools)
	}
	instructions := BuildInstructions("", s.instructionsData())
	if !strings.Contains(instructions, "Work unattended") || !strings.Contains(instructions, "Do not ask the user") {
		t.Fatalf("unattended instructions missing policy: %q", instructions)
	}

	instructions = BuildInstructions("gpt-5.6-sol", s.instructionsData())
	if !strings.Contains(instructions, "GPT 5.6 Sol") || !strings.Contains(instructions, "gpt-5.6-sol") || !strings.Contains(instructions, "## Autonomy and persistence") || !strings.Contains(instructions, "Work unattended") {
		t.Fatalf("gpt unattended instructions missing variant base or addendum: %q", instructions)
	}
}

func TestPlanAndUnattendedAreIndependent(t *testing.T) {
	s := &sessionState{
		parent: &Agent{workspace: &code.Workspace{}},
		toolSet: tool.NewSet(
			tool.Tool{Name: "read", Effect: tool.StaticEffect(tool.EffectReadOnly)},
			tool.Tool{Name: "edit", Effect: tool.StaticEffect(tool.EffectMutates)},
			tool.Tool{Name: "elicit", Effect: tool.StaticEffect(tool.EffectReadOnly)},
		),
	}
	a := upstreamAgent("gpt-6-sol", "gpt-6-astra")
	a.sessions["sid"] = s
	ctx := context.Background()
	if err := a.SetEffort(ctx, "sid", "high"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMode(ctx, "sid", code.PlanModeID); err != nil {
		t.Fatal(err)
	}
	if err := a.SetUnattended(ctx, "sid", true); err != nil {
		t.Fatal(err)
	}

	modes, current := a.Modes("sid")
	if len(modes) != 2 || current != code.PlanModeID || !a.Unattended("sid") {
		t.Fatalf("modes=%v current=%s unattended=%v", modes, current, a.Unattended("sid"))
	}
	if got := s.permissionMode(); got != "plan" {
		t.Fatalf("unattended weakened plan permission mode: %s", got)
	}
	if tools := s.tools(); len(tools) != 1 || tools[0].Name != "read" {
		t.Fatalf("unattended plan tools = %#v", tools)
	}
	if _, got := a.Models("sid"); got != "gpt-6-sol" || a.effortFor(s) != "high" {
		t.Fatalf("policy changed selection: model=%s effort=%s", got, a.effortFor(s))
	}
	if err := a.SetMode(ctx, "sid", code.AgentModeID); err != nil {
		t.Fatal(err)
	}
	if !a.Unattended("sid") || s.permissionMode() != "bypassPermissions" {
		t.Fatal("switching to Agent lost unattended policy")
	}
	if err := a.SetUnattended(ctx, "sid", false); err != nil {
		t.Fatal(err)
	}
	if s.currentMode() != modeAgent || s.permissionMode() != "default" {
		t.Fatal("disabling unattended changed collaboration mode")
	}
}

func TestPlanPreservesModelInstructions(t *testing.T) {
	for _, id := range []string{"gpt-6-sol", "claude-sonnet-5-5"} {
		source, data := instructionTemplate(id, prompt.SectionData{})
		base := prompt.BuildBaseInstructions(source, data)
		source, data = instructionTemplate(id, prompt.SectionData{PlanMode: true})
		plan := prompt.BuildBaseInstructions(source, data)
		if !strings.HasPrefix(plan, base+"\n\n# Plan mode") {
			t.Fatalf("%s Plan discarded model instructions", id)
		}
		if !strings.Contains(plan, "These mode rules take precedence") {
			t.Fatal("Plan is missing explicit precedence over implementation instructions")
		}
		unattended := BuildInstructions(id, prompt.SectionData{PlanMode: true, UnattendedMode: true})
		if !strings.Contains(unattended, "# Plan mode") || !strings.Contains(unattended, "Work unattended") {
			t.Fatal("combined mode/policy lost instructions")
		}
	}
}

func TestPlanRejectsMutatingDynamicToolCalls(t *testing.T) {
	called := false
	dynamic := tool.Tool{
		Name: "exec_command",
		Effect: func(args map[string]any) tool.Effect {
			if args == nil {
				return tool.EffectDynamic
			}
			if args["command"] == "git status" {
				return tool.EffectReadOnly
			}
			return tool.EffectMutates
		},
		Execute: func(context.Context, map[string]any) (tool.Result, error) {
			called = true
			return tool.Text("ran"), nil
		},
	}
	tools := planModeTools([]tool.Tool{dynamic, {Name: "unknown"}})
	if len(tools) != 1 {
		t.Fatalf("plan tools = %#v", tools)
	}
	if _, err := tools[0].Execute(context.Background(), map[string]any{"command": "git status"}); err != nil || !called {
		t.Fatalf("read-only command rejected: %v", err)
	}
	called = false
	if _, err := tools[0].Execute(context.Background(), map[string]any{"command": "go test ./...", "validation": true}); err == nil || called {
		t.Fatalf("unisolated validation bypassed Plan boundary: err=%v called=%v", err, called)
	}
}
