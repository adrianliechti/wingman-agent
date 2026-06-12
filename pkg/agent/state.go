package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type State struct {
	Usage    Usage     `json:"usage"`
	Messages []Message `json:"messages,omitempty"`
	Revision uint64    `json:"-"`
}

func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	data, err := json.Marshal(s)

	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("failed to write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("failed to write state: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("failed to write state: %w", err)
	}

	return nil
}

func (s *State) Load(path string) error {
	data, err := os.ReadFile(path)

	if err != nil {
		return fmt.Errorf("failed to read state: %w", err)
	}

	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("failed to parse state: %w", err)
	}

	return nil
}
