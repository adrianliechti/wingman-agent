package truncation

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// MaxBytes is the default per-tool cap on inline output. Outputs above this
// size are persisted to a scratch file and replaced with a preview envelope.
const MaxBytes = 100 * 1024

// grepMaxBytes is a tighter cap for grep, whose results compound across
// turns more than other tools.
const grepMaxBytes = 20 * 1024

const previewBytes = 2 * 1024

// budgetFor returns the per-tool inline output cap.
func budgetFor(name string) int {
	if name == "grep" {
		return grepMaxBytes
	}
	return MaxBytes
}

// New returns a PostToolUse hook that persists oversized tool output and
// substitutes a preview envelope. Tools that self-cap below their budget
// pass through untouched.
func New(scratchDir string) hook.PostToolUse {
	return func(_ context.Context, call tool.ToolCall, result string) (string, error) {
		budget := budgetFor(call.Name)
		if len(result) <= budget {
			return result, nil
		}
		path := writeScratch(scratchDir, call.Name, result)
		return formatPersisted(len(result), path, result[:previewBytes]), nil
	}
}

func writeScratch(scratchDir, toolName, content string) string {
	if scratchDir == "" {
		return ""
	}
	// os.CreateTemp avoids timestamp collisions under concurrent tool calls.
	f, err := os.CreateTemp(scratchDir, "result-"+sanitizeName(toolName)+"-*.txt")
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return ""
	}
	return f.Name()
}

func formatPersisted(totalBytes int, scratchPath, preview string) string {
	var b strings.Builder
	b.WriteString("<persisted-output>\n")
	fmt.Fprintf(&b, "Output was %d bytes — too large for inline.", totalBytes)
	if scratchPath != "" {
		fmt.Fprintf(&b, " Full output saved to: %s", scratchPath)
	}
	fmt.Fprintf(&b, "\n\nPreview (first %d bytes):\n\n%s", len(preview), preview)
	if scratchPath != "" {
		b.WriteString("\n\nUse `read` on the path above to retrieve specific ranges.")
	}
	b.WriteString("\n</persisted-output>")
	return b.String()
}

func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "tool"
	}
	return string(out)
}
