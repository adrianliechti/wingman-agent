package truncation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

// DefaultMaxBytes is the soft cap applied to tools without a per-tool budget.
// Outputs at or below this size are returned verbatim.
const DefaultMaxBytes = 16 * 1024

const (
	defaultHardBytes    = 64 * 1024
	persistPreviewBytes = 2 * 1024
)

// Budget describes how a tool's output is trimmed. Outputs ≤ SoftBytes pass
// through unchanged. Above SoftBytes but ≤ HardBytes, the output is trimmed
// in-place (head+tail middle-elide, or head-only if HeadBiased) and the full
// text is also saved to scratch so the model can `read` it. Above HardBytes,
// the inline output is replaced with a <persisted-output> envelope containing
// only a small preview plus the scratch path.
type Budget struct {
	SoftBytes  int
	HardBytes  int
	HeadBiased bool
}

// budgetFor returns the budget for a given tool name. Tools not explicitly
// listed fall back to the default soft / hard caps.
func budgetFor(name string) Budget {
	switch name {
	case "shell":
		return Budget{SoftBytes: 8 * 1024, HardBytes: 50 * 1024}
	case "grep":
		return Budget{SoftBytes: 8 * 1024, HardBytes: 50 * 1024, HeadBiased: true}
	case "find":
		return Budget{SoftBytes: 4 * 1024, HardBytes: 32 * 1024, HeadBiased: true}
	case "ls":
		return Budget{SoftBytes: 4 * 1024, HardBytes: 32 * 1024, HeadBiased: true}
	case "read":
		return Budget{SoftBytes: 64 * 1024, HardBytes: 64 * 1024}
	case "fetch":
		return Budget{SoftBytes: 16 * 1024, HardBytes: 100 * 1024}
	case "search_online":
		return Budget{SoftBytes: 8 * 1024, HardBytes: 32 * 1024}
	}
	return Budget{SoftBytes: DefaultMaxBytes, HardBytes: defaultHardBytes}
}

// New returns a PostToolUse hook that trims tool output according to per-tool
// budgets, persisting the full text to scratchDir when the hard cap is hit.
func New(scratchDir string) hook.PostToolUse {
	return func(ctx context.Context, call tool.ToolCall, result string) (string, error) {
		total := len(result)
		b := budgetFor(call.Name)

		if total <= b.SoftBytes {
			return result, nil
		}

		// Hard cap: replace inline with a small preview envelope pointing
		// at a scratch file. Cheaper to carry on every subsequent turn.
		if total > b.HardBytes {
			path := writeScratch(scratchDir, call.Name, result)
			previewBytes := persistPreviewBytes
			if previewBytes > total {
				previewBytes = total
			}
			preview := result[:previewBytes]
			return formatPersisted(total, path, preview), nil
		}

		// Soft cap: inline trim + scratch fallback so the elided portion is
		// retrievable via `read`.
		var trimmed string
		if b.HeadBiased {
			trimmed = text.TruncateHead(result, b.SoftBytes)
		} else {
			trimmed = text.TruncateMiddle(result, b.SoftBytes)
		}

		path := writeScratch(scratchDir, call.Name, result)
		return formatTrimmed(total, path, trimmed), nil
	}
}

func writeScratch(scratchDir, toolName, content string) string {
	if scratchDir == "" {
		return ""
	}
	name := fmt.Sprintf("result-%s-%d.txt", sanitizeName(toolName), time.Now().UnixNano())
	path := filepath.Join(scratchDir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return ""
	}
	return path
}

func formatPersisted(totalBytes int, scratchPath, preview string) string {
	var b string
	b += "<persisted-output>\n"
	b += fmt.Sprintf("Output was %d bytes — too large for inline.", totalBytes)
	if scratchPath != "" {
		b += fmt.Sprintf(" Full output saved to: %s", scratchPath)
	}
	b += "\n\n"
	b += fmt.Sprintf("Preview (first %d bytes):\n\n%s", len(preview), preview)
	if scratchPath != "" {
		b += "\n\nUse `read` on the path above to retrieve specific ranges."
	}
	b += "\n</persisted-output>"
	return b
}

func formatTrimmed(totalBytes int, scratchPath, trimmed string) string {
	var notice string
	if scratchPath != "" {
		notice = fmt.Sprintf("[Output truncated: %d bytes — full output at %s; use `read` on that path for the elided portion.]\n\n", totalBytes, scratchPath)
	} else {
		notice = fmt.Sprintf("[Output truncated: %d bytes. Re-run with a narrower scope if you need more.]\n\n", totalBytes)
	}
	return notice + trimmed
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
