package code

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func TestMouseResizesDiffPanelAcrossDivider(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 220, 20)
	a.diffPanel.diffs = diffPanelFixture
	a.agent.(*uiTestAgent).messages = []agent.Message{{Role: agent.RoleUser, Content: []agent.Content{{Text: strings.Repeat("hello ", 80)}}}}
	a.render()
	before := slices.Clone(a.chat)
	width := a.diffPanelWidth(220)
	dividerX := a.width() + 2
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: dividerX, Y: 5})
	a.handleEvent(inline.MouseEvent{Kind: inline.MouseDrag, X: dividerX - 15, Y: 5})
	if got := a.diffPanelWidth(220); got != width+15 {
		t.Fatalf("panel width after dragging left = %d, want %d", got, width+15)
	}
	if slices.Equal(before, a.chat) {
		t.Fatal("drag did not rewrap the chat")
	}
	a.render()
	// Continue into the pane, crossing the divider's new position.
	a.handleEvent(inline.MouseEvent{Kind: inline.MouseDrag, X: dividerX + 15, Y: 5})
	if got := a.diffPanelWidth(220); got != width-15 {
		t.Fatalf("panel width after dragging right = %d, want %d", got, width-15)
	}
	a.handleEvent(inline.MouseEvent{Kind: inline.MouseRelease, X: dividerX + 15, Y: 5})
	if a.diffPanel.resizing || a.selecting || a.selActive {
		t.Fatal("divider drag left resize or text selection active")
	}
	// Later motion must not continue a released drag.
	a.handleEvent(inline.MouseEvent{Kind: inline.MouseDrag, X: 1, Y: 5})
	if got := a.diffPanelWidth(220); got != width-15 {
		t.Fatalf("released drag changed panel width to %d", got)
	}
}

func TestMouseDiffResizeClampsAndSurvivesToggle(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 180, 20)
	a.diffPanel.diffs = diffPanelFixture
	a.render()
	dividerX := a.width() + 2
	a.handleMouse(inline.MouseEvent{Kind: inline.MousePress, X: dividerX, Y: 5})
	a.handleMouse(inline.MouseEvent{Kind: inline.MouseDrag, X: 1, Y: 5})
	if got := a.width(); got != diffPanelChatMinWidth {
		t.Fatalf("chat shrank to %d, want minimum %d", got, diffPanelChatMinWidth)
	}
	a.handleMouse(inline.MouseEvent{Kind: inline.MouseRelease, X: 180, Y: 5})
	if got := a.diffPanelWidth(180); got != diffPanelMinWidth {
		t.Fatalf("panel shrank to %d, want minimum %d", got, diffPanelMinWidth)
	}
	a.toggleDiffPanel()
	a.toggleDiffPanel()
	if got := a.diffPanelWidth(180); got != diffPanelMinWidth {
		t.Fatalf("toggle lost chosen width: %d", got)
	}
	a.render()
	a.handleMouse(inline.MouseEvent{Kind: inline.MousePress, X: a.width() + 2, Y: 5})
	a.handleEvent(inline.ResizeEvent{Width: 100, Height: 20})
	if a.diffPanel.resizing || a.width() != 100 {
		t.Fatal("terminal resize did not stop dragging and hide the panel")
	}
}

func TestMouseClosesDiffPanelAtRenderedButton(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 180, 20)
	a.diffPanel.diffs = diffPanelFixture
	a.render()
	header := ansi.Strip(a.diffPanel.render(a.diffPanelWidth(180), 20)[0])
	button := strings.Index(header, "×")
	if button < 0 {
		t.Fatal("panel header has no close button")
	}
	x := a.width() + 2 + ansi.Width(header[:button]) + 1
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: x, Y: 2})
	if a.diffPanel.hidden {
		t.Fatal("click below close button hid the panel")
	}
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: x, Y: 1})
	a.render()
	if !a.diffPanel.hidden || a.diffPanelShowing || a.width() != 180 {
		t.Fatal("close button did not restore full-width chat")
	}
}

// Resolve an item from the visible picker text, including the clipping used
// by short terminals, independently of the mouse hit areas.
func clickVisiblePopupLabel(t *testing.T, a *App, label string) {
	t.Helper()
	a.render()
	width, height := a.term.Size()
	lines := a.popup.Render(a.width())
	if height >= 5 {
		lines = append(append([]string{""}, lines...), "")
	}
	if len(lines) > height {
		lines = append([]string{"…"}, lines[len(lines)-height+1:]...)
	}
	for i, line := range lines {
		plain := ansi.Strip(line)
		if index := strings.Index(plain, label); index >= 0 {
			x := ansi.Width(plain[:index]) + 1
			y := height - len(lines) + i + 1
			if x > width {
				t.Fatal("item lies outside terminal")
			}
			a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: x, Y: y})
			return
		}
	}
	t.Fatalf("no visible picker item %q", label)
}

func TestMouseSwitchesModelAndEffort(t *testing.T) {
	for _, width := range []int{40, 180} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			a, _ := newDiffPanelTestApp(t, width, 20)
			a.queue = make(chan func(), 8)
			a.diffPanel.diffs = diffPanelFixture
			backend := a.agent.(*uiTestAgent)
			backend.models = []model.Model{{ID: backend.model, Name: "GPT 5.6 Sol"}, {ID: "other", Name: "Other model"}}
			a.editor.SetText("keep this draft")
			a.render()
			identity := ansi.Strip(a.composerChrome(a.width()).BottomRight)
			x := a.width() - ansi.Width(identity) + 1
			a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: x, Y: 19})
			if a.popup == nil || a.popup.title != "model" {
				t.Fatal("click on composer model did not open picker")
			}
			clickVisiblePopupLabel(t, a, "Other model")
			waitForSessionOperation(t, a)
			if backend.model != "other" || a.popup == nil || a.popup.title != "effort" {
				t.Fatal("model click did not change model and open effort picker")
			}
			clickVisiblePopupLabel(t, a, "high")
			waitForSessionOperation(t, a)
			if backend.effort != "high" || a.popup != nil || a.editor.Text() != "keep this draft" {
				t.Fatal("effort click did not apply selection and preserve draft")
			}
		})
	}
}

func TestMousePopupScrollAndClippedItems(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 5)
	items := make([]PopupItem, 20)
	for i := range items {
		items[i] = PopupItem{ID: fmt.Sprint(i), Label: fmt.Sprintf("choice %02d", i)}
	}
	accepted := ""
	a.popup = newPopup(popupList, "choices", items, func(ids []string) { accepted = ids[0] })
	a.render()
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: 5, Y: 1})
	if accepted != "" {
		t.Fatal("ellipsis row accepted an item")
	}
	a.handleEvent(inline.MouseEvent{Kind: inline.MouseWheel, WheelDelta: 1, X: 5, Y: 2})
	a.handleEvent(inline.MouseEvent{Kind: inline.MouseWheel, WheelDelta: 1, X: 5, Y: 2})
	if a.popup.index != 6 || a.chatScroll != 0 {
		t.Fatal("wheel did not navigate picker independently of chat")
	}
	clickVisiblePopupLabel(t, a, "choice 06")
	if accepted != "6" || a.popup != nil {
		t.Fatalf("clipped picker accepted %q, want 6", accepted)
	}
}

func TestMousePaletteSkipsHeadingAndDisabledItems(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 20)
	accepted := ""
	a.popup = newPopup(popupPalette, "choices", []PopupItem{
		{ID: "disabled", Label: "Disabled choice", Disabled: true},
		{ID: "enabled", Label: "Enabled choice"},
	}, func(ids []string) { accepted = ids[0] })
	clickVisiblePopupLabel(t, a, "choices")
	clickVisiblePopupLabel(t, a, "Disabled choice")
	if accepted != "" || a.popup == nil {
		t.Fatal("heading or disabled row accepted a choice")
	}
	clickVisiblePopupLabel(t, a, "Enabled choice")
	if accepted != "enabled" || a.popup != nil {
		t.Fatal("enabled palette row did not accept choice")
	}
}

func clickFooterLabel(t *testing.T, a *App, label string) {
	t.Helper()
	line := ansi.Strip(a.footerLine(a.width()))
	index := strings.Index(line, label)
	if index < 0 {
		t.Fatalf("footer has no visible %q action: %q", label, line)
	}
	a.render()
	_, height := a.term.Size()
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: ansi.Width(line[:index]) + 1, Y: height})
}

func TestMouseFooterCommandsModesAndTranscript(t *testing.T) {
	for _, width := range []int{80, 180} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			a, _ := newDiffPanelTestApp(t, width, 20)
			a.queue = make(chan func(), 8)
			a.diffPanel.diffs = diffPanelFixture
			a.editor.SetText("keep this draft")
			clickFooterLabel(t, a, "commands")
			if a.popup == nil || a.popup.title != commandCenterTitle {
				t.Fatal("commands click did not open command center")
			}
			a.closePopup()
			clickFooterLabel(t, a, "plan")
			waitForSessionOperation(t, a)
			if a.currentMode() != "plan" {
				t.Fatal("plan click did not enter plan mode")
			}
			clickFooterLabel(t, a, "agent")
			waitForSessionOperation(t, a)
			if a.currentMode() != "agent" {
				t.Fatal("agent click did not return to agent mode")
			}
			clickFooterLabel(t, a, "transcript")
			if _, ok := a.overlay.(*transcriptOverlay); !ok {
				t.Fatal("transcript click did not open inspector")
			}
			if a.editor.Text() != "keep this draft" {
				t.Fatal("footer action changed draft")
			}
		})
	}
}

func TestMouseFooterFilesAndExpandPaste(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 100, 20)
	a.queue = make(chan func(), 8)
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	a.agent.(*uiTestAgent).workspace.Root = root
	clickFooterLabel(t, a, "files")
	select {
	case fn := <-a.queue:
		fn()
	case <-time.After(2 * time.Second):
		t.Fatal("file picker did not load")
	}
	if a.popup == nil || !a.popup.multi || !strings.HasPrefix(a.popup.title, "context") {
		t.Fatal("files click did not open context picker")
	}
	a.closePopup()
	paste := strings.Repeat("pasted text ", 100)
	a.editor.InsertPaste(paste)
	clickFooterLabel(t, a, "expand paste")
	if len(a.editor.pastes) != 0 || a.editor.Text() != paste {
		t.Fatal("expand paste click did not expand the original text")
	}
}

func TestMouseFooterClearsHiddenActions(t *testing.T) {
	a, _ := newDiffPanelTestApp(t, 80, 20)
	a.render()
	// The gap between commands and files has no action.
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: len("ctrl+p commands") + 1, Y: 20})
	if a.popup != nil || a.operationPending {
		t.Fatal("footer gap activated an action")
	}
	a.showToast("tab plan", theme.Default.Foreground)
	a.render()
	if len(a.mouse.footer) != 0 {
		t.Fatal("toast retained hidden footer actions")
	}
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: 30, Y: 20})
	if a.operationPending {
		t.Fatal("toast click activated hidden mode action")
	}
	a.toast = nil
	a.handleEvent(inline.ResizeEvent{Width: 20, Height: 20})
	a.render()
	a.handleEvent(inline.MouseEvent{Kind: inline.MousePress, X: 19, Y: 20})
	if a.popup != nil || len(a.mouse.footer) != 1 {
		t.Fatal("narrow footer retained a clipped action")
	}
	a.showCommandCenter()
	a.render()
	if len(a.mouse.footer) != 0 {
		t.Fatal("picker retained footer actions")
	}
}
