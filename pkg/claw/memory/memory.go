package memory

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	instructionsFile = "AGENTS.md"
	soulFile         = "SOUL.md"
	maxContentBytes  = 25 * 1024
)

// Store manages per-agent and global AGENTS.md instructions for claw.
//
// Directory layout under ~/.wingman/claw/agents/:
//
//	{dir}/
//	  global/
//	    AGENTS.md              -- shared instructions across all agents
//	  {agent}/
//	    AGENTS.md              -- agent-specific instructions
//	    workspace/             -- agent's working directory (files, data)
//	    tasks/                 -- agent's scheduled tasks
type Store struct {
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "global"), 0755); err != nil {
		return nil, err
	}

	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) GlobalDir() string {
	return filepath.Join(s.dir, "global")
}

func (s *Store) AgentDir(name string) string {
	return filepath.Join(s.dir, name)
}

func (s *Store) WorkspaceDir(name string) string {
	return filepath.Join(s.dir, name, "workspace")
}

func (s *Store) TasksDir(name string) string {
	return filepath.Join(s.dir, name, "tasks")
}

func (s *Store) EnsureAgent(name string) error {
	for _, sub := range []string{"", "workspace"} {
		if err := os.MkdirAll(filepath.Join(s.AgentDir(name), sub), 0755); err != nil {
			return err
		}
	}

	soulPath := filepath.Join(s.AgentDir(name), soulFile)

	if _, err := os.Stat(soulPath); os.IsNotExist(err) {
		os.WriteFile(soulPath, []byte(defaultSoul), 0644)
	}

	return nil
}

const defaultSoul = `I solve problems by doing, not by describing what I would do.
I keep responses short unless depth is asked for.
I say what I know, flag what I don't, and never fake confidence.
I treat the user's time as the scarcest resource, and their trust as the most valuable.
`

func (s *Store) RemoveAgent(name string) error {
	return os.RemoveAll(s.AgentDir(name))
}

func (s *Store) ListAgents() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "global" {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func (s *Store) AgentExists(name string) bool {
	info, err := os.Stat(s.AgentDir(name))
	return err == nil && info.IsDir()
}

func (s *Store) GlobalContent() string {
	return readFileTruncated(filepath.Join(s.GlobalDir(), instructionsFile))
}

func (s *Store) AgentContent(name string) string {
	return readFileTruncated(filepath.Join(s.WorkspaceDir(name), instructionsFile))
}

// Content concatenates global and agent-specific instructions.
func (s *Store) Content(name string) string {
	global := s.GlobalContent()
	local := s.AgentContent(name)

	if global == "" {
		return local
	}

	if local == "" {
		return global
	}

	return global + "\n\n---\n\n" + local
}

func (s *Store) WriteGlobal(content string) error {
	return os.WriteFile(filepath.Join(s.GlobalDir(), instructionsFile), []byte(content), 0644)
}

// SoulContent reads SOUL.md from outside the workspace, where the agent cannot modify it.
func (s *Store) SoulContent(name string) string {
	return readFileTruncated(filepath.Join(s.AgentDir(name), soulFile))
}

func (s *Store) WriteAgent(name string, content string) error {
	return os.WriteFile(filepath.Join(s.WorkspaceDir(name), instructionsFile), []byte(content), 0644)
}

func readFileTruncated(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		return ""
	}

	if len(content) > maxContentBytes {
		truncated := content[:maxContentBytes]
		if idx := strings.LastIndex(truncated, "\n"); idx > 0 {
			truncated = truncated[:idx]
		}

		content = truncated + "\n\n> WARNING: File exceeded 25KB and was truncated."
	}

	return content
}
