package code

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type composerState struct {
	Text     string          `json:"text"`
	Content  []agent.Content `json:"content,omitempty"`
	Files    []string        `json:"files,omitempty"`
	QueueID  string          `json:"queueId,omitempty"`
	Previous *composerState  `json:"previous,omitempty"`
}

func (d composerState) clone() composerState {
	d.Content = agent.CloneContent(d.Content)
	d.Files = slices.Clone(d.Files)
	if d.Previous != nil {
		previous := *d.Previous
		previous.Previous = nil
		previous = previous.clone()
		d.Previous = &previous
	}
	return d
}

type draftStore struct {
	mu                sync.Mutex
	persistMu         sync.Mutex
	path              string
	drafts            map[string]composerState
	revision, written uint64
	loadErr           error
}

func newDraftStore(workspace *code.Workspace, backend string) *draftStore {
	store := &draftStore{drafts: map[string]composerState{}}
	if workspace == nil || workspace.MemoryPath == "" {
		return store
	}
	store.path = filepath.Join(filepath.Dir(workspace.MemoryPath), "tui-drafts", fmt.Sprintf("%x.json", sha256.Sum256([]byte(backend))))
	data, err := os.ReadFile(store.path)
	if err == nil {
		err = json.Unmarshal(data, &store.drafts)
	}
	if err != nil && !os.IsNotExist(err) {
		store.loadErr = err
	}
	if store.drafts == nil {
		store.drafts = map[string]composerState{}
	}
	return store
}

func (s *draftStore) Get(id string) composerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drafts[id].clone()
}
func (s *draftStore) Set(id string, draft composerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reflect.DeepEqual(s.drafts[id], draft) {
		return
	}
	if draft.Text == "" && len(draft.Content) == 0 && len(draft.Files) == 0 && draft.Previous == nil {
		delete(s.drafts, id)
	} else {
		s.drafts[id] = draft.clone()
	}
	s.revision++
}

// Only the writer is serialized across disk I/O. Input updates never wait on it,
// and an older writer always snapshots the latest revision before writing.
func (s *draftStore) Flush() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	for {
		s.mu.Lock()
		if s.loadErr != nil {
			s.mu.Unlock()
			return s.loadErr
		}
		if s.path == "" || s.written == s.revision {
			s.mu.Unlock()
			return nil
		}
		revision := s.revision
		data, err := json.Marshal(s.drafts)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(s.path), ".draft-*")
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		err = errors.Join(writeErr, syncErr, closeErr)
		if err == nil {
			err = os.Rename(file.Name(), s.path)
		}
		_ = os.Remove(file.Name())
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.written = revision
		s.mu.Unlock()
	}
}

func (a *App) currentDraft() composerState {
	draft := composerState{Content: a.pendingContent, Files: a.pendingFiles, QueueID: a.editingQueueID, Previous: a.beforeQueueEdit}
	if a.editor != nil {
		draft.Text = a.editor.Text()
	}
	return draft
}
func (a *App) restoreDraft(draft composerState) {
	if a.editor != nil {
		a.editor.SetText(draft.Text)
	}
	a.pendingContent = agent.CloneContent(draft.Content)
	a.pendingFiles = slices.Clone(draft.Files)
	a.editingQueueID = draft.QueueID
	a.beforeQueueEdit = draft.clone().Previous
}
func (a *App) markDraftChanged() {
	if !a.askActive {
		a.draftPending = true
		a.draftChangedAt = time.Now()
	}
}
func (a *App) saveCurrentDraft() {
	if a.drafts == nil || a.askActive {
		return
	}
	a.drafts.Set(a.sessionID, a.currentDraft())
	a.draftPending = false
	go func() {
		if err := a.drafts.Flush(); err != nil {
			a.post(func() { a.showToast("Could not save draft: "+err.Error(), theme.Default.Red) })
		}
	}()
}
func (a *App) flushDraftWhenIdle(now time.Time) {
	if a.draftPending && now.Sub(a.draftChangedAt) >= 350*time.Millisecond {
		a.saveCurrentDraft()
	}
}
