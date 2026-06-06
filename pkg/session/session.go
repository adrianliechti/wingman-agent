package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
)

type Session struct {
	ID        string      `json:"id"`
	Title     string      `json:"title,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	State     agent.State `json:"state"`
}

type record struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	Message   *agent.Message `json:"message,omitempty"`
	Usage     *agent.Usage   `json:"usage,omitempty"`
}

type persisted struct {
	count    int
	revision uint64
}

var (
	persistedMu sync.Mutex
	persistedBy = map[string]persisted{}
)

func Save(sessionsDir string, id string, state agent.State) error {
	if sessionsDir == "" {
		return fmt.Errorf("no sessions directory available")
	}

	if len(state.Messages) == 0 {
		return nil
	}

	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return fmt.Errorf("failed to create sessions directory: %w", err)
	}

	path := filepath.Join(sessionsDir, id+".jsonl")

	persistedMu.Lock()
	p, ok := persistedBy[path]
	persistedMu.Unlock()

	appended := false

	if ok && p.revision == state.Revision && len(state.Messages) >= p.count {
		appended = appendFile(path, state.Messages[p.count:], state.Usage) == nil
	}

	if !appended {
		if err := rewriteFile(path, id, state); err != nil {
			return err
		}

		os.Remove(filepath.Join(sessionsDir, id+".json"))
	}

	persistedMu.Lock()
	persistedBy[path] = persisted{count: len(state.Messages), revision: state.Revision}
	persistedMu.Unlock()

	return nil
}

func Load(sessionsDir string, id string) (Session, error) {
	if sessionsDir == "" {
		return Session{}, fmt.Errorf("no sessions directory available")
	}

	path := filepath.Join(sessionsDir, id+".jsonl")

	if s, err := loadLines(path, id, false); err == nil {
		persistedMu.Lock()
		persistedBy[path] = persisted{count: len(s.State.Messages)}
		persistedMu.Unlock()

		return s, nil
	}

	return loadFile(filepath.Join(sessionsDir, id+".json"))
}

func List(sessionsDir string) ([]Session, error) {
	dir := sessionsDir
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
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

		s, err := loadLines(filepath.Join(dir, entry.Name()), id, true)
		if err != nil {
			continue
		}

		seen[id] = true
		sessions = append(sessions, s)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		if seen[strings.TrimSuffix(entry.Name(), ".json")] {
			continue
		}

		s, err := loadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}

		s.State.Messages = nil
		sessions = append(sessions, s)
	}

	slices.SortFunc(sessions, func(a, b Session) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})

	return sessions, nil
}

func Delete(sessionsDir string, id string) error {
	dir := sessionsDir
	if dir == "" {
		return nil
	}

	path := filepath.Join(sessionsDir, id+".jsonl")

	persistedMu.Lock()
	delete(persistedBy, path)
	persistedMu.Unlock()

	err := os.Remove(path)

	if legacyErr := os.Remove(filepath.Join(dir, id+".json")); legacyErr == nil || err == nil {
		return nil
	}

	return err
}

func appendFile(path string, messages []agent.Message, usage agent.Usage) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	for i := range messages {
		if err := enc.Encode(record{Type: "message", Message: &messages[i]}); err != nil {
			return err
		}
	}

	if err := enc.Encode(record{Type: "state", Usage: &usage}); err != nil {
		return err
	}

	_, err = f.Write(buf.Bytes())
	return err
}

func rewriteFile(path, id string, state agent.State) error {
	createdAt := time.Now()

	if existing, err := loadLines(path, id, true); err == nil && !existing.CreatedAt.IsZero() {
		createdAt = existing.CreatedAt
	} else if legacy, err := loadFile(strings.TrimSuffix(path, ".jsonl") + ".json"); err == nil && !legacy.CreatedAt.IsZero() {
		createdAt = legacy.CreatedAt
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	if err := enc.Encode(record{Type: "meta", ID: id, CreatedAt: &createdAt}); err != nil {
		return err
	}

	for i := range state.Messages {
		if err := enc.Encode(record{Type: "message", Message: &state.Messages[i]}); err != nil {
			return err
		}
	}

	if err := enc.Encode(record{Type: "state", Usage: &state.Usage}); err != nil {
		return err
	}

	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	return nil
}

func loadLines(path, id string, infoOnly bool) (Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, fmt.Errorf("failed to read session file: %w", err)
	}
	defer f.Close()

	s := Session{ID: id}

	dec := json.NewDecoder(f)

	for {
		var r record
		if err := dec.Decode(&r); err != nil {
			break
		}

		switch r.Type {
		case "meta":
			if r.ID != "" {
				s.ID = r.ID
			}
			if r.CreatedAt != nil {
				s.CreatedAt = *r.CreatedAt
			}
		case "message":
			if r.Message == nil {
				continue
			}
			if s.Title == "" {
				s.Title = messageTitle(*r.Message)
			}
			if infoOnly {
				if s.Title != "" {
					if info, err := os.Stat(path); err == nil {
						s.UpdatedAt = info.ModTime()
					}
					return s, nil
				}
				continue
			}
			s.State.Messages = append(s.State.Messages, *r.Message)
		case "state":
			if r.Usage != nil {
				s.State.Usage = *r.Usage
			}
		}
	}

	if s.CreatedAt.IsZero() && len(s.State.Messages) == 0 && !infoOnly {
		return Session{}, fmt.Errorf("failed to parse session file")
	}

	if info, err := os.Stat(path); err == nil {
		s.UpdatedAt = info.ModTime()
	}

	if s.CreatedAt.IsZero() {
		s.CreatedAt = s.UpdatedAt
	}

	return s, nil
}

func messageTitle(m agent.Message) string {
	if m.Hidden || m.Role != "user" {
		return ""
	}

	for _, c := range m.Content {
		if c.Text == "" {
			continue
		}
		title := c.Text

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

func extractTitle(messages []agent.Message) string {
	for _, m := range messages {
		if title := messageTitle(m); title != "" {
			return title
		}
	}
	return ""
}

func loadFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, fmt.Errorf("failed to read session file: %w", err)
	}

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, fmt.Errorf("failed to parse session file: %w", err)
	}

	if len(s.State.Messages) == 0 && s.State.Usage == (agent.Usage{}) {
		var legacy struct {
			Messages []agent.Message `json:"messages,omitempty"`
			Usage    agent.Usage     `json:"usage"`
		}

		if err := json.Unmarshal(data, &legacy); err == nil {
			s.State = agent.State{
				Messages: legacy.Messages,
				Usage:    legacy.Usage,
			}
		}
	}

	if s.Title == "" {
		s.Title = extractTitle(s.State.Messages)
	}

	return s, nil
}
