package code

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	corecode "github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
)

func TestDraftStorePersistsAttachmentsAndDoesNotResurrectSentDraft(t *testing.T) {
	dir := t.TempDir()
	workspace := &corecode.Workspace{MemoryPath: filepath.Join(dir, "memory")}
	store := newDraftStore(workspace, "backend")
	draft := composerState{Text: "unsent", Files: []string{"main.go"}, Content: []agent.Content{{Text: "attachment context"}}}
	store.Set("session", draft)
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	restored := newDraftStore(workspace, "backend")
	if got := restored.Get("session"); !reflect.DeepEqual(got, draft) {
		t.Fatalf("draft = %+v", got)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := store.Flush(); err != nil {
				t.Error(err)
			}
		})
	}
	store.Set("session", composerState{})
	wg.Go(func() {
		if err := store.Flush(); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	if got := newDraftStore(workspace, "backend").Get("session"); got.Text != "" || len(got.Files) != 0 {
		t.Fatalf("sent draft resurrected: %+v", got)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("draft permissions = %v", info.Mode())
	}
}

func TestSwitchingSessionsRestoresTheirSeparateDrafts(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	app.editor.SetText("first session")
	app.pendingFiles = []string{"first.go"}
	app.activateSession("second")
	if app.editor.Text() != "" || len(app.pendingFiles) != 0 {
		t.Fatal("draft leaked into another session")
	}
	app.editor.SetText("second session")
	app.activateSession("session")
	if app.editor.Text() != "first session" || !reflect.DeepEqual(app.pendingFiles, []string{"first.go"}) {
		t.Fatal("first draft was lost")
	}
	app.activateSession("second")
	if app.editor.Text() != "second session" {
		t.Fatal("second draft was lost")
	}
}

func TestQuestionTemporarilyOwnsComposerAndRestoresDraft(t *testing.T) {
	app, _ := newQueueTestApp(t, 1)
	app.editor.SetText("unsent user instructions")
	app.pendingFiles = []string{"original.go"}
	app.runQueueCommand("edit x")
	app.editor.SetText("updated queued instructions")
	done := make(chan string, 1)
	go func() { answer, _ := app.askLineLocked(t.Context(), "Which target?", "answer"); done <- answer }()
	select {
	case fn := <-app.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("question never opened")
	}
	if !app.askActive || app.editor.Text() != "" || len(app.pendingFiles) != 0 {
		t.Fatal("question inherited separate draft")
	}
	if app.editingQueueID != "" || app.beforeQueueEdit != nil {
		t.Error("question inherited queue edit controls")
	}
	app.editor.SetText("chosen target")
	app.handleKey(inline.KeyEvent{Key: inline.KeyEnter})
	select {
	case got := <-done:
		if got != "chosen target" {
			t.Fatalf("answer = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("question never answered")
	}
	select {
	case fn := <-app.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("draft never restored")
	}
	if app.askActive || app.editingQueueID != "x" || app.editor.Text() != "updated queued instructions" {
		t.Fatal("answering question lost queue edit")
	}
	app.endQueueEdit()
	if app.editor.Text() != "unsent user instructions" || !reflect.DeepEqual(app.pendingFiles, []string{"original.go"}) {
		t.Fatal("answering question lost draft")
	}
}

func TestQueueEditKeepsSeparateDraftAcrossSessionSwitch(t *testing.T) {
	app, _ := newQueueTestApp(t, 1)
	app.editor.SetText("original unsent prompt")
	app.pendingFiles = []string{"original.go"}
	app.runQueueCommand("edit x")
	app.editor.SetText("updated queue input")
	app.activateSession("second")
	app.activateSession("session")
	if app.editingQueueID != "x" || app.editor.Text() != "updated queue input" {
		t.Fatal("queue edit was not recovered")
	}
	app.endQueueEdit()
	if app.editor.Text() != "original unsent prompt" || !reflect.DeepEqual(app.pendingFiles, []string{"original.go"}) {
		t.Fatal("queue recovery lost original draft")
	}
}
