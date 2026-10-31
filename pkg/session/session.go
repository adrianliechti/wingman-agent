package session

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

const journalVersion = 2

type Session struct {
	ID        string        `json:"id"`
	Title     string        `json:"title,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	State     harness.State `json:"state"`
}

type record struct {
	Type      string                `json:"type"`
	Version   int                   `json:"version,omitempty"`
	ID        string                `json:"id,omitempty"`
	CreatedAt *time.Time            `json:"created_at,omitempty"`
	Title     string                `json:"title,omitempty"`
	Event     *harness.RuntimeEvent `json:"event,omitempty"`
}

// Journal is the live append boundary for one session. All instances for the
// same path share a process-local lock, so a final Save and live Agent writes
// cannot interleave JSON records.
type Journal struct {
	path  string
	id    string
	coord *journalCoordinator
}

type journalCoordinator struct {
	mu           sync.Mutex
	initialized  bool
	deleted      bool
	lastSequence uint64
	createdAt    time.Time
}

var journalCoordinators sync.Map

func journalCoord(path string) *journalCoordinator {
	coord, _ := journalCoordinators.LoadOrStore(filepath.Clean(path), &journalCoordinator{})
	return coord.(*journalCoordinator)
}

func sessionPath(dir, id string) string { return filepath.Join(dir, id+".jsonl") }

// ArtifactDir is the durable sidecar directory for state that is not part of
// conversational history (turn queue, schedules, and subagent registry).
func ArtifactDir(sessionsDir, id string) (string, error) {
	if sessionsDir == "" {
		return "", fmt.Errorf("no sessions directory available")
	}
	if err := validateSessionID(id); err != nil {
		return "", err
	}
	return filepath.Join(sessionsDir, id+".data"), nil
}

func OpenJournal(sessionsDir, id string) (*Journal, error) {
	if sessionsDir == "" {
		return nil, fmt.Errorf("no sessions directory available")
	}
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	path := sessionPath(sessionsDir, id)
	coord := journalCoord(path)
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if coord.deleted {
		return nil, fmt.Errorf("session journal %s was deleted", id)
	}
	if err := ensureCurrentFormat(sessionsDir, id); err != nil {
		return nil, err
	}
	if err := repairIncompleteTail(path); err != nil {
		return nil, err
	}
	if !coord.initialized {
		coord.createdAt = time.Now().UTC()
		if loaded, err := loadCurrent(path, id, true); err == nil {
			coord.lastSequence = loaded.lastSequence
			if !loaded.session.CreatedAt.IsZero() {
				coord.createdAt = loaded.session.CreatedAt
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		coord.initialized = true
	}
	return &Journal{path: path, id: id, coord: coord}, nil
}

func (j *Journal) AppendEvents(events []harness.RuntimeEvent) error {
	if len(events) == 0 {
		return nil
	}
	j.coord.mu.Lock()
	defer j.coord.mu.Unlock()
	return j.appendLocked(events, false)
}

func (j *Journal) appendMissing(events []harness.RuntimeEvent) error {
	j.coord.mu.Lock()
	defer j.coord.mu.Unlock()
	return j.appendLocked(events, true)
}

func (j *Journal) appendLocked(events []harness.RuntimeEvent, skipExisting bool) error {
	if j.coord.deleted {
		return fmt.Errorf("session journal %s was deleted", j.id)
	}
	var pending []harness.RuntimeEvent
	for _, event := range events {
		if skipExisting && event.Sequence <= j.coord.lastSequence {
			continue
		}
		want := j.coord.lastSequence + uint64(len(pending)) + 1
		if event.Sequence != want {
			return fmt.Errorf("runtime event sequence %d is not the next journal sequence %d", event.Sequence, want)
		}
		pending = append(pending, event)
	}
	if len(pending) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(j.path), 0755); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}
	_, statErr := os.Stat(j.path)
	newFile := os.IsNotExist(statErr)
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open session journal: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if newFile {
		created := j.coord.createdAt
		if err := enc.Encode(record{Type: "meta", Version: journalVersion, ID: j.id, CreatedAt: &created}); err != nil {
			f.Close()
			return err
		}
	}
	for i := range pending {
		if err := enc.Encode(record{Type: "event", Event: &pending[i]}); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("append session journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync session journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close session journal: %w", err)
	}
	j.coord.lastSequence = pending[len(pending)-1].Sequence
	return nil
}

// Save is a compatibility/backstop API. Live built-in sessions append through
// Journal as facts happen; Save only fills events absent from the file.
func Save(sessionsDir, id string, state harness.State) error {
	if sessionsDir == "" {
		return fmt.Errorf("no sessions directory available")
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	normalized, err := normalizeStateForSave(state)
	if err != nil {
		return err
	}
	if len(normalized.Events) == 0 {
		return nil
	}
	j, err := OpenJournal(sessionsDir, id)
	if err != nil {
		return err
	}
	return j.appendMissing(normalized.Events)
}

// Fork copies id's journal, not its sidecar artifacts, into newID; title defaults to "Fork of <source>".
func Fork(sessionsDir, id, newID, title string) error {
	return fork(sessionsDir, id, newID, title, "")
}

// ForkThrough forks history up to the next user input after inputID, so the
// fork ends with that input's turn.
func ForkThrough(sessionsDir, id, newID, title, inputID string) error {
	if inputID == "" {
		return fmt.Errorf("input id required")
	}
	return fork(sessionsDir, id, newID, title, inputID)
}

func cutThrough(events []harness.RuntimeEvent, inputID string) ([]harness.RuntimeEvent, bool) {
	input := func(e harness.RuntimeEvent) string {
		if e.Type == harness.EventMessage && e.Message != nil && e.Message.Role == harness.RoleUser {
			return e.Message.InputID
		}
		return ""
	}
	start := slices.IndexFunc(events, func(e harness.RuntimeEvent) bool { return input(e) == inputID })
	if start < 0 {
		return nil, false
	}
	for i := start + 1; i < len(events); i++ {
		// A steering input shares its turn; loading closes the cut turn as interrupted.
		if events[i].Type == harness.EventTurnStarted || input(events[i]) != "" && input(events[i]) != inputID {
			return events[:i], true
		}
	}
	return events, true
}

func fork(sessionsDir, id, newID, title, through string) error {
	if sessionsDir == "" {
		return fmt.Errorf("no sessions directory available")
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	if err := validateSessionID(newID); err != nil {
		return err
	}
	if err := ensureCurrentFormat(sessionsDir, id); err != nil {
		return err
	}
	source := sessionPath(sessionsDir, id)
	sourceCoord := journalCoord(source)
	sourceCoord.mu.Lock()
	loaded, err := loadCurrent(source, id, false)
	sourceCoord.mu.Unlock()
	if err != nil {
		return err
	}
	events := loaded.session.State.Events
	if through != "" {
		var ok bool
		if events, ok = cutThrough(events, through); !ok {
			return fmt.Errorf("session %s has no input %s", id, through)
		}
	}
	if len(events) == 0 {
		return fmt.Errorf("session %s has no history to fork", id)
	}
	if title == "" {
		title = "Fork of " + cmp.Or(loaded.session.Title, "untitled session")
	}

	path := sessionPath(sessionsDir, newID)
	coord := journalCoord(path)
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("session %s already exists", newID)
	} else if !os.IsNotExist(err) {
		return err
	}

	created := time.Now().UTC()
	if err := writeJournal(path, record{Type: "meta", Version: journalVersion, ID: newID, CreatedAt: &created, Title: title}, events); err != nil {
		return fmt.Errorf("write forked session journal: %w", err)
	}
	coord.initialized = true
	coord.createdAt = created
	coord.lastSequence = events[len(events)-1].Sequence
	return nil
}

// writeJournal atomically replaces path with a journal of meta and events.
func writeJournal(path string, meta record, events []harness.RuntimeEvent) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	err = enc.Encode(meta)
	for i := 0; err == nil && i < len(events); i++ {
		err = enc.Encode(record{Type: "event", Event: &events[i]})
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func Load(sessionsDir, id string) (Session, error) {
	if sessionsDir == "" {
		return Session{}, fmt.Errorf("no sessions directory available")
	}
	if err := validateSessionID(id); err != nil {
		return Session{}, err
	}
	if err := ensureCurrentFormat(sessionsDir, id); err != nil {
		return Session{}, err
	}
	loaded, err := loadCurrent(sessionPath(sessionsDir, id), id, false)
	if err != nil {
		return Session{}, err
	}
	return loaded.session, nil
}

func List(sessionsDir string) ([]Session, error) {
	if sessionsDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	seen := map[string]bool{}
	var sessions []Session
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		if validateSessionID(id) != nil {
			continue
		}
		path := filepath.Join(sessionsDir, entry.Name())
		var s Session
		if current, err := isCurrentJournal(path); err == nil && current {
			loaded, err := loadCurrent(path, id, true)
			if err != nil {
				continue
			}
			s = loaded.session
		} else {
			legacy, err := loadLegacyJSONL(path, id)
			if err != nil {
				continue
			}
			s = legacy
			s.State = harness.State{}
		}
		seen[id] = true
		sessions = append(sessions, s)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if validateSessionID(id) != nil {
			continue
		}
		if seen[id] {
			continue
		}
		s, err := loadLegacyJSON(filepath.Join(sessionsDir, entry.Name()))
		if err != nil {
			continue
		}
		s.State = harness.State{}
		sessions = append(sessions, s)
	}
	slices.SortFunc(sessions, func(a, b Session) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return sessions, nil
}

func Delete(sessionsDir, id string) error {
	if sessionsDir == "" {
		return nil
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	artifactDir, err := ArtifactDir(sessionsDir, id)
	if err != nil {
		return err
	}
	// Serialize deletion with append, and invalidate every existing writer.
	// Cancelling a turn does not stop its deferred terminal events immediately.
	key := filepath.Clean(sessionPath(sessionsDir, id))
	coord := journalCoord(key)
	coord.mu.Lock()
	defer coord.mu.Unlock()
	coord.deleted = true
	defer journalCoordinators.CompareAndDelete(key, coord)
	paths := []string{sessionPath(sessionsDir, id), filepath.Join(sessionsDir, id+".json")}
	var errs []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(artifactDir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validateSessionID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.IsAbs(id) || filepath.Base(id) != id || strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("invalid session ID %q", id)
	}
	return nil
}

type currentLoad struct {
	session      Session
	lastSequence uint64
}

func loadCurrent(path, fallbackID string, infoOnly bool) (currentLoad, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return currentLoad{}, fmt.Errorf("failed to read session journal: %w", err)
	}

	s := Session{ID: fallbackID}
	var events []harness.RuntimeEvent
	lines := bytes.Split(data, []byte{'\n'})
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			if i == len(lines)-1 && len(data) > 0 && data[len(data)-1] != '\n' {
				break
			}
			return currentLoad{}, fmt.Errorf("parse session journal line %d: %w", i+1, err)
		}
		switch r.Type {
		case "meta":
			if r.Version != journalVersion {
				return currentLoad{}, fmt.Errorf("unsupported session journal version %d", r.Version)
			}
			if r.ID != "" {
				s.ID = r.ID
			}
			if r.CreatedAt != nil {
				s.CreatedAt = *r.CreatedAt
			}
			if r.Title != "" {
				s.Title = r.Title
			}
		case "event":
			if r.Event == nil {
				return currentLoad{}, fmt.Errorf("session journal line %d has no event", i+1)
			}
			events = append(events, *r.Event)
			if s.Title == "" && r.Event.Type == harness.EventMessage && r.Event.Message != nil {
				s.Title = messageTitle(*r.Event.Message)
			}
		default:
			return currentLoad{}, fmt.Errorf("unknown session journal record %q", r.Type)
		}
	}
	if s.CreatedAt.IsZero() && len(events) == 0 {
		return currentLoad{}, fmt.Errorf("failed to parse session journal")
	}
	if stat, err := os.Stat(path); err == nil {
		s.UpdatedAt = stat.ModTime()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = s.UpdatedAt
	}
	last := uint64(0)
	if len(events) > 0 {
		last = events[len(events)-1].Sequence
	}
	if !infoOnly {
		state, err := normalizeState(harness.State{Events: events})
		if err != nil {
			return currentLoad{}, err
		}
		s.State = state
	}
	return currentLoad{session: s, lastSequence: last}, nil
}

// repairIncompleteTail removes a torn final record, or terminates a complete
// one missing its newline. Loading and reopening must preserve the same facts.
func repairIncompleteTail(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	cut := bytes.LastIndexByte(data, '\n') + 1
	if json.Valid(data[cut:]) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte{'\n'})
		if err == nil {
			err = f.Sync()
		}
		return errors.Join(err, f.Close())
	}
	if cut == 0 {
		return fmt.Errorf("session journal has no complete record")
	}
	if err := os.Truncate(path, int64(cut)); err != nil {
		return fmt.Errorf("repair incomplete session journal tail: %w", err)
	}
	return nil
}

func normalizeState(state harness.State) (harness.State, error) {
	var runtime harness.Agent
	if err := runtime.Restore(state); err != nil {
		return harness.State{}, fmt.Errorf("restore session state: %w", err)
	}
	return runtime.StateSnapshot(), nil
}

// normalizeStateForSave preserves the old State-based API for callers that
// still append to State.Messages directly. Existing ledger history must be an
// exact prefix; rewrites are rejected because new journals are append-only.
func normalizeStateForSave(state harness.State) (harness.State, error) {
	normalized, err := normalizeState(state)
	if err != nil {
		return harness.State{}, err
	}
	if len(state.Events) == 0 {
		return normalized, nil
	}
	events := append([]harness.RuntimeEvent(nil), state.Events...)
	sequence := uint64(0)
	if len(events) > 0 {
		sequence = events[len(events)-1].Sequence
	}
	if state.Messages != nil {
		if len(state.Messages) < len(normalized.Messages) || !equalMessages(state.Messages[:len(normalized.Messages)], normalized.Messages) {
			return harness.State{}, fmt.Errorf("canonical session history cannot be rewritten")
		}
		for _, message := range state.Messages[len(normalized.Messages):] {
			sequence++
			copy := harness.CloneMessages([]harness.Message{message})[0]
			events = append(events, harness.RuntimeEvent{
				Sequence: sequence, ID: fmt.Sprintf("state-message-%d", sequence),
				Type: harness.EventMessage, At: time.Now().UTC(), Message: &copy,
			})
		}
	}
	if state.ContextSet && (!normalized.ContextSet || !equalMessages(state.Context, normalized.Context)) {
		sequence++
		events = append(events, harness.RuntimeEvent{
			Sequence: sequence, ID: fmt.Sprintf("state-context-%d", sequence),
			Type: harness.EventContextCheckpoint, At: time.Now().UTC(),
			Context: harness.CloneMessages(state.Context), ContextReason: "State compatibility checkpoint",
		})
	}
	if state.Usage != normalized.Usage {
		delta := harness.Usage{
			InputTokens:              state.Usage.InputTokens - normalized.Usage.InputTokens,
			OutputTokens:             state.Usage.OutputTokens - normalized.Usage.OutputTokens,
			ReasoningTokens:          state.Usage.ReasoningTokens - normalized.Usage.ReasoningTokens,
			CacheReadInputTokens:     state.Usage.CacheReadInputTokens - normalized.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: state.Usage.CacheCreationInputTokens - normalized.Usage.CacheCreationInputTokens,
			LastInputTokens:          state.Usage.LastInputTokens,
			ContextWindow:            state.Usage.ContextWindow,
		}
		if delta.InputTokens < 0 || delta.OutputTokens < 0 || delta.ReasoningTokens < 0 || delta.CacheReadInputTokens < 0 || delta.CacheCreationInputTokens < 0 {
			return harness.State{}, fmt.Errorf("cumulative session usage cannot decrease")
		}
		sequence++
		events = append(events, harness.RuntimeEvent{
			Sequence: sequence, ID: fmt.Sprintf("state-usage-%d", sequence),
			Type: harness.EventUsage, At: time.Now().UTC(), Usage: &delta,
		})
	}
	return normalizeState(harness.State{Events: events})
}

func equalMessages(left, right []harness.Message) bool {
	a, err := json.Marshal(left)
	if err != nil {
		return false
	}
	b, err := json.Marshal(right)
	return err == nil && bytes.Equal(a, b)
}

func isCurrentJournal(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var header record
	if err := dec.Decode(&header); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return header.Type == "meta" && header.Version == journalVersion, nil
}

func messageTitle(m harness.Message) string {
	if m.Hidden || m.Role != harness.RoleUser {
		return ""
	}
	for _, c := range m.Content {
		if c.Text == "" {
			continue
		}
		title := c.Text
		if strings.HasPrefix(strings.TrimSpace(title), "<user_shell_command>") {
			if start := strings.Index(title, "<command>"); start >= 0 {
				if end := strings.Index(title[start:], "</command>"); end >= 0 {
					cmd := strings.TrimSpace(title[start+len("<command>") : start+end])
					if cmd != "" {
						title = "! " + cmd
					}
				}
			}
		}
		if idx := strings.IndexAny(title, "\n\r"); idx >= 0 {
			title = title[:idx]
		}
		title = strings.TrimSpace(title)
		if len(title) > 80 {
			title = title[:77] + "..."
		}
		if title != "" {
			return title
		}
	}
	return ""
}

func extractTitle(messages []harness.Message) string {
	for _, message := range messages {
		if title := messageTitle(message); title != "" {
			return title
		}
	}
	return ""
}
