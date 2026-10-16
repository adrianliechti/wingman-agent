package code

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/clipboard"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestNormalizePastedText(t *testing.T) {
	got := normalizePastedText("one\r\ntwo\rthree\x00\x07\tfour \x1b[31mred\x1b[0m")
	if want := "one\ntwo\nthree\tfour red"; got != want {
		t.Fatalf("normalizePastedText() = %q, want %q", got, want)
	}
}

func TestHandlePasteRoutesStandalonePopupQuery(t *testing.T) {
	a := &App{
		agent:  newUITestAgent(nil),
		editor: NewEditor(),
		popup:  newPopup(popupList, "Pick", []PopupItem{{ID: "1", Label: "hello world"}}, nil),
	}

	a.handlePaste("hello\r\n world\x00")

	if got := a.popup.query; got != "hello world" {
		t.Fatalf("popup query = %q, want %q", got, "hello world")
	}
	if got := a.editor.Text(); got != "" {
		t.Fatalf("editor text = %q, want empty", got)
	}
}

func TestHandlePasteSeparatesExistingPopupQuery(t *testing.T) {
	popup := newPopup(popupList, "Pick", []PopupItem{{ID: "1", Label: "hello world"}}, nil)
	popup.SetQuery("hello")
	a := &App{agent: newUITestAgent(nil), editor: NewEditor(), popup: popup}

	a.handlePaste("world")

	if got := popup.query; got != "hello world" {
		t.Fatalf("popup query = %q, want %q", got, "hello world")
	}
}

func TestHandlePasteRoutesFocusedOverlaySearch(t *testing.T) {
	overlay := newTwoPaneOverlay("Problems", "", 1, nil, nil, func(int) string { return "nee dle" })
	overlay.searching = true
	a := &App{agent: newUITestAgent(nil), editor: NewEditor(), overlay: overlay}

	a.handlePaste("nee\r\ndle")

	if got := overlay.query; got != "nee dle" {
		t.Fatalf("overlay query = %q, want %q", got, "nee dle")
	}
	if len(overlay.filtered) != 1 {
		t.Fatalf("filtered = %v, want one match", overlay.filtered)
	}
	if got := a.editor.Text(); got != "" {
		t.Fatalf("editor text = %q, want empty", got)
	}
}

func TestHandlePasteDoesNotLeakThroughOverlay(t *testing.T) {
	overlay := newTwoPaneOverlay("Problems", "", 0, nil, nil, nil)
	a := &App{agent: newUITestAgent(nil), editor: NewEditor(), overlay: overlay}

	a.handlePaste("hidden")

	if got := a.editor.Text(); got != "" {
		t.Fatalf("editor text = %q, want empty", got)
	}
}

func TestTranscriptSearchAcceptsPaste(t *testing.T) {
	overlay := &transcriptOverlay{
		searching: true,
		selected:  0,
		entries: []transcriptEntry{{
			kind: transcriptAssistant, raw: "hello world", selectable: true,
		}},
	}

	if !overlay.HandlePaste("hello\r\nworld") {
		t.Fatal("focused transcript search did not handle paste")
	}
	if got := overlay.query; got != "hello world" {
		t.Fatalf("query = %q, want %q", got, "hello world")
	}
	if len(overlay.matches) != 1 {
		t.Fatalf("matches = %v, want one match", overlay.matches)
	}
}

func TestAskPasteStaysText(t *testing.T) {
	a := &App{agent: newUITestAgent(nil), editor: NewEditor(), askActive: true}
	a.handlePaste("./answer.txt")
	if got := a.editor.Text(); got != "./answer.txt" {
		t.Fatalf("editor text = %q, want path as answer text", got)
	}
	if len(a.pendingFiles) != 0 {
		t.Fatalf("pending files = %v, want none", a.pendingFiles)
	}
}

func TestClipboardImageDoesNotAttachBehindPopup(t *testing.T) {
	image := "data:image/png;base64,aW1hZ2U="
	a := &App{
		agent:  newUITestAgent(nil),
		editor: NewEditor(),
		popup:  newPopup(popupList, "Pick", nil, nil),
	}
	a.applyClipboardContents([]clipboard.Content{{Image: &image}}, nil)
	if len(a.pendingContent) != 0 {
		t.Fatalf("pending content = %v, want none", a.pendingContent)
	}
}

func TestPasteFromClipboardUsesInjectedReader(t *testing.T) {
	a := &App{
		agent:  newUITestAgent(nil),
		editor: NewEditor(),
		queue:  make(chan func(), 1),
		quit:   make(chan struct{}),
		clipboardRead: func() ([]clipboard.Content, error) {
			return []clipboard.Content{{Text: "one\r\ntwo\x00"}}, nil
		},
	}

	a.pasteFromClipboard()
	select {
	case fn := <-a.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("clipboard callback was not posted")
	}

	if got := a.editor.Text(); got != "one\ntwo" {
		t.Fatalf("editor text = %q, want normalized paste", got)
	}
}

func TestClipboardShortcutUsesFocusedInput(t *testing.T) {
	for _, target := range []string{"composer", "picker", "overlay search"} {
		for _, alt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/alt=%t", target, alt), func(t *testing.T) {
				a := &App{
					agent: newUITestAgent(nil), editor: NewEditor(),
					queue: make(chan func(), 1), quit: make(chan struct{}),
					clipboardRead: func() ([]clipboard.Content, error) {
						return []clipboard.Content{{Text: "hello\r\nworld"}}, nil
					},
				}
				switch target {
				case "picker":
					a.popup = newPopup(popupList, "Pick", nil, nil)
				case "overlay search":
					overlay := newTwoPaneOverlay("Search", "", 0, nil, nil, nil)
					overlay.searching = true
					a.overlay = overlay
				}
				a.handleKey(inline.KeyEvent{Key: inline.KeyCtrl, Rune: 'v', Alt: alt})
				select {
				case fn := <-a.queue:
					fn()
				case <-time.After(time.Second):
					t.Fatal("paste shortcut was swallowed before reading the clipboard")
				}
				switch target {
				case "composer":
					if a.editor.Text() != "hello\nworld" {
						t.Fatalf("composer = %q", a.editor.Text())
					}
				case "picker":
					if a.popup.query != "hello world" || a.editor.Text() != "" {
						t.Fatalf("picker = %q; hidden composer = %q", a.popup.query, a.editor.Text())
					}
				case "overlay search":
					if a.overlay.(*twoPaneOverlay).query != "hello world" || a.editor.Text() != "" {
						t.Fatal("paste did not stay in the focused overlay search")
					}
				}
			})
		}
	}
}

func TestDelayedClipboardDoesNotPasteIntoAnotherTarget(t *testing.T) {
	for _, target := range []string{"session", "popup", "overlay", "prompt"} {
		t.Run(target, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			a := &App{
				agent: newUITestAgent(nil), editor: NewEditor(), sessionID: "first",
				queue: make(chan func(), 1), quit: make(chan struct{}),
				clipboardRead: func() ([]clipboard.Content, error) {
					close(started)
					<-release
					return []clipboard.Content{{Text: "first target only"}}, nil
				},
			}
			a.pasteFromClipboard()
			<-started
			switch target {
			case "session":
				a.sessionID = "second"
			case "popup":
				a.popup = newPopup(popupList, "Pick", nil, nil)
			case "overlay":
				a.overlay = newTwoPaneOverlay("Search", "", 0, nil, nil, nil)
			case "prompt":
				a.askActive = true
				a.askResponse = make(chan string, 1)
			}
			close(release)
			select {
			case fn := <-a.queue:
				fn()
			case <-time.After(time.Second):
				t.Fatal("paste did not complete")
			}
			if a.editor.Text() != "" || (a.popup != nil && a.popup.query != "") {
				t.Fatal("delayed clipboard text was inserted into another target")
			}
		})
	}
}

func TestApplyClipboardContentsShowsErrorsAndEmptyState(t *testing.T) {
	a := &App{}
	a.applyClipboardContents(nil, errors.New("unavailable"))
	if a.toast == nil || a.toast.message != "Clipboard paste failed: unavailable" {
		t.Fatalf("error toast = %#v", a.toast)
	}

	a.applyClipboardContents(nil, nil)
	if a.toast == nil || a.toast.message != "Clipboard is empty" {
		t.Fatalf("empty toast = %#v", a.toast)
	}
}

func TestLastAssistantTextIncludesAllVisibleBlocks(t *testing.T) {
	messages := []agent.Message{
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: "older"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{ToolCall: &agent.ToolCall{Name: "read"}}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{
			{Text: "first"},
			{Text: "secret", Hidden: true},
			{ToolCall: &agent.ToolCall{Name: "shell"}},
			{Text: "second"},
		}},
	}

	if got := lastAssistantText(messages); got != "firstsecond" {
		t.Fatalf("lastAssistantText() = %q", got)
	}
}

func TestLastAssistantTextSkipsToolOnlyMessage(t *testing.T) {
	messages := []agent.Message{
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: "answer"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{ToolCall: &agent.ToolCall{Name: "read"}}}},
	}
	if got := lastAssistantText(messages); got != "answer" {
		t.Fatalf("lastAssistantText() = %q, want answer", got)
	}
}

func TestLastAssistantTextSkipsWhitespaceOnlyMessage(t *testing.T) {
	messages := []agent.Message{
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: "answer"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: " \n\t"}}},
	}
	if got := lastAssistantText(messages); got != "answer" {
		t.Fatalf("lastAssistantText() = %q, want answer", got)
	}
}

func TestCopyLastResponseUsesInjectedWriter(t *testing.T) {
	var written string
	a := &App{
		agent: newUITestAgent([]agent.Message{{
			Role:    agent.RoleAssistant,
			Content: []agent.Content{{Text: "first"}, {Text: "second"}},
		}}),
		clipboardWrite: func(text string) error {
			written = text
			return nil
		},
	}

	a.copyLastResponse()
	if written != "firstsecond" {
		t.Fatalf("copied text = %q", written)
	}
}

func TestCopyLastResponseShowsMissingResponse(t *testing.T) {
	a := &App{agent: newUITestAgent(nil)}
	a.copyLastResponse()
	if a.toast == nil || a.toast.message != "No assistant response to copy" {
		t.Fatalf("toast = %#v", a.toast)
	}
}

func TestCopyLastResponseShowsBackendError(t *testing.T) {
	a := &App{
		agent: newUITestAgent([]agent.Message{{
			Role: agent.RoleAssistant, Content: []agent.Content{{Text: "answer"}},
		}}),
		clipboardWrite: func(string) error { return errors.New("unavailable") },
	}
	a.copyLastResponse()
	if a.toast == nil || a.toast.message != "Clipboard copy failed: unavailable" {
		t.Fatalf("toast = %#v", a.toast)
	}
}

func TestDetectFilePathsMultiFileFormats(t *testing.T) {
	dir := t.TempDir()
	imgOne := filepath.Join(dir, "one.png")
	imgTwo := filepath.Join(dir, "Screen Shot 2.png")
	if err := os.WriteFile(imgOne, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgTwo, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}

	escapedTwo := strings.ReplaceAll(imgTwo, " ", `\ `)
	if got := detectFilePaths(imgOne+" "+escapedTwo, dir); !reflect.DeepEqual(got, []string{imgOne, imgTwo}) {
		t.Fatalf("escaped drag-and-drop paths = %#v, want %#v", got, []string{imgOne, imgTwo})
	}

	quoted := fmt.Sprintf("'%s' \"%s\"", imgOne, imgTwo)
	if got := detectFilePaths(quoted, dir); !reflect.DeepEqual(got, []string{imgOne, imgTwo}) {
		t.Fatalf("quoted paths = %#v, want %#v", got, []string{imgOne, imgTwo})
	}

	uriList := fmt.Sprintf("# comment\nfile://%s\nfile://%s\n", filepath.ToSlash(imgOne), strings.ReplaceAll(filepath.ToSlash(imgTwo), " ", "%20"))
	if got := detectFilePaths(uriList, dir); !reflect.DeepEqual(got, []string{imgOne, imgTwo}) {
		t.Fatalf("uri-list paths = %#v, want %#v", got, []string{imgOne, imgTwo})
	}
}

func TestSubmitAttachedImageFileIncludesVisionContentAndAllowsEmptyPrompt(t *testing.T) {
	a, _ := newQueueTestApp(t, 1)
	a.agent.Workspace().RootPath = t.TempDir()

	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(a.agent.Workspace().RootPath, "diagram.png")
	if err := os.WriteFile(imgPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.addFileToContext("diagram.png"); err != nil {
		t.Fatal(err)
	}
	a.submitInputIntent(code.TurnInputFollowUp)

	snapshot := a.turns.Snapshot("session")
	if len(snapshot.Inputs) != 2 {
		t.Fatalf("queued turns = %d, want 2", len(snapshot.Inputs))
	}
	sub := snapshot.Inputs[1].Input
	if sub.Display == nil || sub.Display.Text != "[diagram.png]" {
		t.Fatalf("display = %#v, want [diagram.png]", sub.Display)
	}
	var sawVision bool
	for _, part := range sub.Content {
		if part.File != nil && part.File.Name == "diagram.png" && strings.HasPrefix(part.File.Data, "data:image/png;base64,") {
			sawVision = true
		}
	}
	if !sawVision {
		t.Fatalf("expected attached image file to include vision File content, got %#v", sub.Content)
	}
}

func TestApplyClipboardContentsMultiImages(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	encoded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())

	a := &App{agent: newUITestAgent(nil)}
	a.applyClipboardContents([]clipboard.Content{
		{Image: &encoded},
		{Image: &encoded},
	}, nil)
	if got := a.countPendingImages(); got != 2 {
		t.Fatalf("countPendingImages() = %d, want 2", got)
	}
	if a.toast == nil || a.toast.message != "Attached 2 items" {
		t.Fatalf("toast = %#v, want Attached 2 items", a.toast)
	}
}

