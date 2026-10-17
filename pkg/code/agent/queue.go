package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

const turnQueueFile = "turn-queue.json"

func (a *Agent) turnQueuePath(sessionID string) (string, error) {
	artifactDir, err := session.ArtifactDir(a.sessionsDir, sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(artifactDir, turnQueueFile), nil
}

func (a *Agent) LoadTurnQueue(sessionID string) (code.TurnQueueState, error) {
	path, err := a.turnQueuePath(sessionID)
	if err != nil {
		return code.TurnQueueState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return code.TurnQueueState{}, nil
		}
		return code.TurnQueueState{}, err
	}
	var state code.TurnQueueState
	if err := json.Unmarshal(data, &state); err != nil {
		return code.TurnQueueState{}, fmt.Errorf("parse turn queue: %w", err)
	}
	return state, nil
}

func (a *Agent) SaveTurnQueue(sessionID string, state code.TurnQueueState) error {
	path, err := a.turnQueuePath(sessionID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Dir(path), turnQueueFile, data)
}
