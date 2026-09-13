package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const (
	trimProtectTokens   = 24 * 1024
	trimProtectMessages = 12
	trimResultThreshold = 1024
	trimResultKeepBytes = 256
)

const trimMarker = "\n[earlier tool output trimmed to reclaim context — rerun the tool if it is needed again]"
const trimImageMarker = "[image result trimmed to reclaim context — rerun the tool if it is needed again]"

// compressContext reclaims superseded host snapshots first, then stale tool
// results if needed. It commits only when summarization can be avoided.
func (a *Agent) compressContext(requiredTokens int64) (int, error) {
	messages := a.contextSnapshot()
	before := messagesTokens(messages)
	rewritten := false
	if latest := lastSessionContextIndex(messages); latest > 0 {
		older := slices.DeleteFunc(messages[:latest], isSessionContext)
		rewritten = len(older) < latest
		messages = append(older, messages[latest:]...)
	}

	cut := 0
	total := 0
	for i, v := range slices.Backward(messages) {
		if total > trimProtectTokens && len(messages)-i > trimProtectMessages {
			cut = i + 1
			break
		}
		total += messageTokens(v)
	}

	// This snapshot is owned: clear signatures once, after choosing the
	// protected window, so savings include the payloads any rewrite removes.
	clearReplayableReasoning(messages)
	// Reclaiming obsolete snapshots alone may suffice. Leave tool evidence
	// intact in that case, including the most recent tool/reasoning cycle.
	if requiredTokens > 0 && rewritten && int64(before-messagesTokens(messages)) >= requiredTokens {
		cut = 0
	}

	for i := range messages[:cut] {
		m := &messages[i]
		if m.Role != RoleAssistant {
			continue
		}

		content := slices.DeleteFunc(m.Content, func(c Content) bool { return c.File != nil })
		imageDropped := len(content) < len(m.Content)
		rewritten = rewritten || imageDropped
		m.Content = content
		for _, c := range content {
			if result := c.ToolResult; result != nil {
				if len(result.Content) > trimResultThreshold {
					result.Content = trimmedToolOutput(result.Content)
					rewritten = true
				}
				if imageDropped {
					result.Content += "\n" + trimImageMarker
				}
			}
		}
	}

	if !rewritten {
		return 0, nil
	}
	freed := max(0, before-messagesTokens(messages))
	// If compression cannot avoid a summary pass, let the summarizer see
	// the original evidence and commit only its final checkpoint.
	if int64(freed) < requiredTokens {
		return 0, nil
	}
	if err := a.replaceContext("compress stale context", messages); err != nil {
		return 0, err
	}
	return freed, nil
}

func trimmedToolOutput(output string) string {
	stub := text.HeadBytes(output, trimResultKeepBytes)
	// Persisted output remains retrievable even when the preview is reclaimed.
	if _, path, ok := strings.Cut(output, "Full output saved to: "); ok {
		path, _, _ = strings.Cut(path, "\n")
		if !strings.Contains(stub, "Full output saved to: "+path) {
			stub += "\nFull output saved to: " + path
		}
	}
	return stub + trimMarker
}

const (
	// The code harness installs a more specific PostToolUse hook that persists
	// oversized output before replacing it with a preview. This is the final
	// safety net for every other Agent embedding and for context added by hooks.
	maxInlineToolResultBytes = 48 * 1024
	toolResultHeadBytes      = 4 * 1024
	toolResultTailBytes      = 8 * 1024
)

func boundToolResult(content string) string {
	if len(content) <= maxInlineToolResultBytes {
		return content
	}
	head := text.HeadBytes(content, toolResultHeadBytes)
	tail := text.TailBytes(content, toolResultTailBytes)
	omitted := len(content) - len(head) - len(tail)
	return fmt.Sprintf(
		"<truncated-output>\nOutput was %d bytes (%d lines); %d bytes omitted. Output was truncated by the agent harness.\n\nPreview (first %d bytes):\n\n%s\n\n[... %d bytes omitted ...]\n\nPreview (last %d bytes):\n\n%s\n</truncated-output>",
		len(content), text.LineCount(content), omitted, len(head), head, omitted, len(tail), tail,
	)
}
