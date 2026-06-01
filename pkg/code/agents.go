package code

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// BuiltinAgentName identifies the in-process wingman backend. It's
// reserved — user-defined [AgentDef] entries cannot share this name.
const BuiltinAgentName = "wingman"

// AgentDef describes an external ACP-server backend. The selected
// backend is launched as a subprocess; its stdio carries the ACP
// JSON-RPC stream. Wingman talks to it as a client (analogous to Zed /
// other ACP hosts).
//
// Defs are loaded from ~/.wingman/agents.json by [LoadAgents]; the
// built-in wingman backend (name = [BuiltinAgentName]) is always
// available and doesn't need a config entry.
type AgentDef struct {
	// Name is the user-facing label and the key used by [*Agent.SetAgent].
	Name string `json:"name"`

	// Command is the absolute path (or PATH-lookup name) of the ACP
	// server binary to spawn.
	Command string `json:"command"`

	// Args are extra arguments passed to the subprocess.
	Args []string `json:"args,omitempty"`

	// Env adds/overrides environment variables for the subprocess. The
	// parent process's environment is inherited.
	Env map[string]string `json:"env,omitempty"`
}

// agentsConfigPath is the on-disk location of the agents config.
// Same dir as the rest of wingman's user config so users only manage
// one ~/.wingman/ tree.
func agentsConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wingman", "agents.json"), nil
}

// HasAgentsConfig reports whether ~/.wingman/agents.json exists. When it
// does it is treated as the authoritative agent list, so built-in CLI
// auto-detection is skipped and only the wingman backend plus the file's
// own entries are offered.
func HasAgentsConfig() bool {
	path, err := agentsConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// LoadAgents reads ~/.wingman/agents.json and returns the configured
// external coder backends. A missing or unreadable file means "only the
// built-in wingman backend is available." Entries without a Name or
// Command are dropped silently so a malformed line doesn't break the
// selection list.
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
			// Reserved — user shouldn't be able to shadow the in-process
			// backend with a subprocess of the same name.
			continue
		}
		out = append(out, d)
	}
	return out
}

// SaveAgents writes the agents config. Used by the desktop app's
// settings page. Creating the parent directory is handled here so
// callers don't have to.
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
