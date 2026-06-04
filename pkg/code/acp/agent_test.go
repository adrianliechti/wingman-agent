package acp

import (
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

func TestToolCallContentTextRendersDiff(t *testing.T) {
	old := "line one\nold line\nline three\n"
	items := []acpsdk.ToolCallContent{
		{Diff: &acpsdk.ToolCallContentDiff{
			Type:    "diff",
			Path:    "/p/a.go",
			OldText: &old,
			NewText: "line one\nnew line\nline three\n",
		}},
	}
	got := toolCallContentText(items)
	if !strings.Contains(got, "/p/a.go") {
		t.Errorf("expected path in output:\n%s", got)
	}
	if !strings.Contains(got, "-old line") || !strings.Contains(got, "+new line") {
		t.Errorf("expected -/+ diff lines:\n%s", got)
	}
	if !strings.Contains(got, " line one") {
		t.Errorf("expected unchanged context line:\n%s", got)
	}
}

func TestToolCallContentTextAddedFile(t *testing.T) {

	items := []acpsdk.ToolCallContent{
		{Diff: &acpsdk.ToolCallContentDiff{
			Type:    "diff",
			Path:    "/p/new.go",
			NewText: "package main\n\nfunc main() {}\n",
		}},
	}
	got := toolCallContentText(items)
	if strings.Contains(got, "-") {
		t.Errorf("added file should have no removed lines:\n%s", got)
	}
	if !strings.Contains(got, "+package main") || !strings.Contains(got, "+func main() {}") {
		t.Errorf("expected all lines added:\n%s", got)
	}
}

func TestToolCallContentTextPlainText(t *testing.T) {
	items := []acpsdk.ToolCallContent{
		{Content: &acpsdk.ToolCallContentContent{Content: acpsdk.TextBlock("hello output")}},
	}
	if got := toolCallContentText(items); got != "hello output" {
		t.Errorf("plain text = %q", got)
	}
}

func modeState(modes ...string) *acpsdk.SessionModeState {
	avail := make([]acpsdk.SessionMode, 0, len(modes))
	for _, m := range modes {
		avail = append(avail, acpsdk.SessionMode{Id: acpsdk.SessionModeId(m), Name: m})
	}
	return &acpsdk.SessionModeState{
		AvailableModes: avail,
		CurrentModeId:  acpsdk.SessionModeId(modes[0]),
	}
}

func TestModesPerSession(t *testing.T) {
	a := &Agent{sessions: map[string]*sessionState{}}
	add := func(id string, modes ...string) {
		s := &sessionState{id: acpsdk.SessionId(id)}
		s.applyModes(modeState(modes...))
		a.sessions[id] = s
	}
	add("a", "plan", "code")
	add("b", "code", "plan")

	if modes, cur := a.Modes("a"); cur != "plan" || len(modes) != 2 {
		t.Fatalf("session a = (%v, %q), want 2 modes current plan", modes, cur)
	}
	if _, cur := a.Modes("b"); cur != "code" {
		t.Fatalf("session b current = %q, want code", cur)
	}
	if modes, cur := a.Modes("missing"); modes != nil || cur != "" {
		t.Fatalf("unknown session = (%v, %q), want (nil, \"\")", modes, cur)
	}
}
