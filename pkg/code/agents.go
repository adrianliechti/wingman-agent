package code

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const BuiltinAgentName = "wingman"

type AgentDef struct {
	Name string `json:"name"`

	Command string `json:"command"`

	Args []string `json:"args,omitempty"`

	Env map[string]string `json:"env,omitempty"`
}

func agentsConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wingman", "agents.json"), nil
}

func HasAgentsConfig() bool {
	path, err := agentsConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func LoadAgents() []AgentDef {
	path, err := agentsConfigPath()
	if err != nil {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var defs []AgentDef
	if err := json.Unmarshal(data, &defs); err != nil {
		return nil
	}

	out := make([]AgentDef, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" || d.Command == "" {
			continue
		}
		if d.Name == BuiltinAgentName {

			continue
		}
		out = append(out, d)
	}
	return out
}

func SaveAgents(defs []AgentDef) error {
	path, err := agentsConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func findAgentDef(defs []AgentDef, name string) (AgentDef, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return AgentDef{}, false
}
