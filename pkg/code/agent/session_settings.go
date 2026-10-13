package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

const sessionSettingsFile = "settings.json"

// sessionSettings is the per-session model selection that must survive a
// restart; conversational history alone does not record an explicit choice.
type sessionSettings struct {
	Model  string  `json:"model,omitempty"`
	Effort *string `json:"effort,omitempty"`
}

func (a *Agent) loadSessionSettings(id string) (sessionSettings, error) {
	dir, err := session.ArtifactDir(a.sessionsDir, id)
	if err != nil {
		return sessionSettings{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, sessionSettingsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return sessionSettings{}, nil
		}
		return sessionSettings{}, err
	}
	var settings sessionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return sessionSettings{}, fmt.Errorf("parse session settings: %w", err)
	}
	return settings, nil
}

// saveSessionSettingsLocked persists the session's selection. Callers hold
// modelMu so concurrent changes are written in the order they were applied.
func (a *Agent) saveSessionSettingsLocked(s *sessionState) error {
	if s == nil || a.sessionsDir == "" {
		return nil
	}
	dir, err := session.ArtifactDir(a.sessionsDir, s.id)
	if err != nil {
		return err
	}
	data, err := json.Marshal(sessionSettings{Model: s.modelID, Effort: s.effort})
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, sessionSettingsFile, data)
}

// restoreSessionSettings applies saved settings, falling back to the model of
// the last turn for sessions recorded before settings were persisted.
func (a *Agent) restoreSessionSettings(s *sessionState, state harness.State) {
	settings, err := a.loadSessionSettings(s.id)
	if err != nil {
		settings = sessionSettings{}
	}
	if settings.Model == "" {
		settings.Model = lastTurnModel(state.Events)
	}
	s.modelID = settings.Model
	s.effort = settings.Effort
}

func lastTurnModel(events []harness.RuntimeEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == harness.EventTurnStarted && events[i].Model != "" {
			return events[i].Model
		}
	}
	return ""
}

func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
