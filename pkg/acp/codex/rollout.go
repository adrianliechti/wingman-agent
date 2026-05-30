package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// rolloutCommandOutputs recovers per-command output for a resumed thread.
//
// codex's `thread/resume` returns historical commandExecution items with
// aggregatedOutput=null, so replayed command tool calls would otherwise show
// no output. The output is still on disk in codex's rollout log
// (~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<threadID>.jsonl) as
// function_call_output entries keyed by call_id, which matches the resumed
// commandExecution item id. This reads that log and maps call_id -> output.
//
// Returns nil when the rollout can't be found or read; replay then falls back
// to showing no output, which is the prior behaviour.
func rolloutCommandOutputs(sessionID string) map[string]string {
	if sessionID == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := findRolloutFile(filepath.Join(home, ".codex", "sessions"), sessionID)
	if path == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	out := parseRolloutOutputs(f)
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseRolloutOutputs reads a codex rollout log and maps each
// function_call_output's call_id to its output text.
func parseRolloutOutputs(r io.Reader) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var line struct {
			Payload struct {
				Type   string          `json:"type"`
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		p := line.Payload
		if p.Type != "function_call_output" || p.CallID == "" {
			continue
		}
		if text := decodeRolloutOutput(p.Output); text != "" {
			out[p.CallID] = text
		}
	}
	return out
}

// findRolloutFile locates the rollout log whose file name ends with the thread
// id (rollout files are named rollout-<timestamp>-<threadID>.jsonl).
func findRolloutFile(root, sessionID string) string {
	suffix := sessionID + ".jsonl"
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), suffix) {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// decodeRolloutOutput renders a function_call_output payload into displayable
// text. codex writes it as a JSON string wrapped in an exec envelope
// ("Wall time: …\nOutput:\n<payload>"); MCP tool results carry their payload as
// a JSON array of content blocks. This unwraps both so the result reads as
// plain text instead of raw JSON.
func decodeRolloutOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	s = strings.TrimSpace(s)

	// Drop codex's exec envelope (metadata lines before "Output:").
	if strings.HasPrefix(s, "Chunk ID:") || strings.HasPrefix(s, "Wall time:") {
		if i := strings.Index(s, "\nOutput:\n"); i >= 0 {
			s = s[i+len("\nOutput:\n"):]
		}
	}

	// MCP results are a JSON array of content blocks; flatten to their text.
	if text, ok := flattenContentBlocks(s); ok {
		return text
	}
	return s
}

// flattenContentBlocks joins the text of a JSON array of {type,text} content
// blocks. ok is false when s isn't such an array.
func flattenContentBlocks(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(t), &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}
