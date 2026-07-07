package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const maxWorkspaces = 3

type Settings struct {
	WingmanURL   string   `json:"url"`
	WingmanToken string   `json:"token"`
	LargeContext bool     `json:"large_context,omitempty"`
	Workspaces   []string `json:"workspaces,omitempty"`
}

func (s *Settings) AddWorkspace(path string) {
	if path == "" {
		return
	}

	filtered := make([]string, 0, len(s.Workspaces)+1)
	filtered = append(filtered, path)
	for _, p := range s.Workspaces {
		if p == path {
			continue
		}
		filtered = append(filtered, p)
	}

	if len(filtered) > maxWorkspaces {
		filtered = filtered[:maxWorkspaces]
	}

	s.Workspaces = filtered
}

func (s *Settings) RemoveWorkspace(path string) {
	filtered := make([]string, 0, len(s.Workspaces))
	for _, p := range s.Workspaces {
		if p == path {
			continue
		}
		filtered = append(filtered, p)
	}

	s.Workspaces = filtered
}

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".wingman", "config.json"), nil
}

func loadSettings() (Settings, error) {
	var s Settings

	path, err := settingsPath()
	if err != nil {
		return s, err
	}

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}

	if len(data) > 0 {
		if err := json.Unmarshal(data, &s); err != nil {
			return s, err
		}
	}

	if s.WingmanURL == "" {
		s.WingmanURL = os.Getenv("WINGMAN_URL")
	}

	if s.WingmanToken == "" {
		s.WingmanToken = os.Getenv("WINGMAN_TOKEN")
	}

	return s, nil
}

func saveSettings(s Settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}

func (s Settings) Apply() {
	os.Setenv("WINGMAN_URL", s.WingmanURL)
	os.Setenv("WINGMAN_TOKEN", s.WingmanToken)

	if s.LargeContext {
		os.Setenv("WINGMAN_LARGE_CONTEXT", "1")
	}
}
