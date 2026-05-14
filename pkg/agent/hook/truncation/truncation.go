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

const DefaultMaxBytes = 16 * 1024

func New(maxBytes int, scratchDir string) hook.PostToolUse {
	return func(ctx context.Context, call tool.ToolCall, result string) (string, error) {
		if len(result) <= maxBytes {
			return result, nil
		}

		totalBytes := len(result)
		truncated := text.TruncateMiddle(result, maxBytes)

		var notice string

		if scratchDir != "" {
			name := fmt.Sprintf("result-%d.txt", time.Now().UnixNano())
			path := filepath.Join(scratchDir, name)

			if err := os.WriteFile(path, []byte(result), 0644); err == nil {
				notice = fmt.Sprintf("[Output truncated: %d bytes — head and tail kept, middle elided. Full output saved to %s; use `read` on that path to retrieve a specific range.]\n\n", totalBytes, path)
			}
		}

		if notice == "" {
			notice = fmt.Sprintf("[Output truncated: %d bytes — head and tail kept, middle elided. Re-run with a narrower scope if you need the omitted section.]\n\n", totalBytes)
		}

		return notice + truncated, nil
	}
}
