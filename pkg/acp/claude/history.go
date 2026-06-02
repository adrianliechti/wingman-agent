package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
)

// The Claude CLI persists each conversation to
//   ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl
// where <encoded-cwd> is the absolute cwd with every non-alphanumeric
// character replaced by '-' (truncated to 200 chars + suffix when longer,
// which we don't replicate here — that suffix is a hash we can't compute).
// The file is a newline-delimited stream of mixed event types; we only care
// about a few of them for listing/replay.

const maxProjectKeyLen = 200

// projectsRoot returns ~/.claude/projects, or "" if HOME is not resolvable.
func projectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// encodeCwd replaces every non-alphanumeric byte with '-', matching the
// CLI's project-key encoding. Returns "" for an empty input.
func encodeCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	b := make([]byte, len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b[i] = c
		default:
			b[i] = '-'
		}
	}
	if len(b) > maxProjectKeyLen {
		// The CLI appends a hash for over-long paths; we don't reproduce it.
		// Returning the truncated prefix still matches whatever bucket the
		// CLI created when cwd was under the limit, which is the common case.
		b = b[:maxProjectKeyLen]
	}
	return string(b)
}

// projectDirFor returns the on-disk directory the CLI uses for cwd, or ""
// if cwd / HOME is missing. Symlinks are resolved so that callers that pass
// `/tmp/x` line up with the CLI's `/private/tmp/x` on macOS.
func projectDirFor(cwd string) string {
	if cwd == "" {
		return ""
	}
	root := projectsRoot()
	if root == "" {
		return ""
	}
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	return filepath.Join(root, encodeCwd(resolved))
}

// historyHeader is the subset of fields we read while scanning a session file
// to build SessionInfo. The CLI writes one of these per line; unknown lines
// are ignored.
type historyHeader struct {
	Type    string               `json:"type"`
	AITitle string               `json:"aiTitle,omitempty"`
	Cwd     string               `json:"cwd,omitempty"`
	Message historyHeaderMessage `json:"message,omitempty"`
}

type historyHeaderMessage struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

// listProjectSessions returns SessionInfo for every .jsonl session file the
// CLI has written. If cwd is non-empty, results are scoped to that project's
// directory; if cwd is empty, every project under ~/.claude/projects is
// scanned. Newer sessions come first.
func listProjectSessions(cwd string) ([]acp.SessionInfo, error) {
	dirs, err := projectDirs(cwd)
	if err != nil {
		return nil, err
	}
	out := make([]acp.SessionInfo, 0)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			title, recordedCwd := scanSessionMetadata(filepath.Join(dir, name))
			sessCwd := recordedCwd
			if sessCwd == "" {
				sessCwd = cwd
			}
			if sessCwd == "" {
				// Cwd is required on SessionInfo; skip orphans where we have
				// neither a request cwd nor a recorded one.
				continue
			}
			ts := info.ModTime().UTC().Format(time.RFC3339)
			si := acp.SessionInfo{
				SessionId: acp.SessionId(id),
				Cwd:       sessCwd,
				UpdatedAt: &ts,
			}
			if title != "" {
				si.Title = &title
			}
			out = append(out, si)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Both updatedAt values are populated above; newest first.
		return *out[i].UpdatedAt > *out[j].UpdatedAt
	})
	return out, nil
}

// projectDirs returns the on-disk dirs to scan for cwd. When cwd is empty,
// every project under ~/.claude/projects is enumerated.
func projectDirs(cwd string) ([]string, error) {
	if cwd != "" {
		d := projectDirFor(cwd)
		if d == "" {
			return nil, nil
		}
		return []string{d}, nil
	}
	root := projectsRoot()
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(root, e.Name()))
	}
	return out, nil
}

// deleteProjectSession removes the on-disk session file for id. The delete
// request carries no cwd, so every project dir is scanned; a missing file is
// not an error (delete is idempotent).
func deleteProjectSession(id acp.SessionId) error {
	dirs, err := projectDirs("")
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := os.Remove(filepath.Join(dir, string(id)+".jsonl")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// sessionExists reports whether ~/.claude/projects/<cwd>/<id>.jsonl is on disk.
func sessionExists(cwd string, id acp.SessionId) bool {
	dir := projectDirFor(cwd)
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, string(id)+".jsonl"))
	return err == nil
}

// scanSessionMetadata reads a session file looking for an `ai-title` event
// (preferred) and falls back to the first user message. It also returns the
// recorded cwd from the first message that carries one.
func scanSessionMetadata(path string) (title, cwd string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var firstUserText string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var h historyHeader
		if err := json.Unmarshal(line, &h); err != nil {
			continue
		}
		if cwd == "" && h.Cwd != "" {
			cwd = h.Cwd
		}
		switch h.Type {
		case "ai-title":
			if h.AITitle != "" {
				return h.AITitle, cwd
			}
		case "user":
			if firstUserText == "" {
				if text := firstTextFromContent(h.Message.Content); text != "" {
					firstUserText = text
				}
			}
		}
	}
	if firstUserText != "" {
		return truncateTitle(firstUserText), cwd
	}
	return "", cwd
}

// firstTextFromContent extracts the first non-empty text payload from a
// message.content value, which may be either a bare string or an array of
// content blocks.
func firstTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []cliMsgBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

func truncateTitle(s string) string {
	const max = 80
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// replayHistory streams the contents of an existing session file out as ACP
// session updates so the client can rebuild the transcript before the user
// sends their first new prompt. Best-effort: malformed lines are skipped.
func replayHistory(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, cwd string) error {
	dir := projectDirFor(cwd)
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, string(sid)+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return streamHistory(ctx, conn, sid, cwd, f)
}

func streamHistory(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, cwd string, r io.Reader) error {
	cache := toolUseCache{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env cliEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch env.Type {
		case "user":
			if err := replayUserMessage(ctx, conn, sid, env.Message, cache); err != nil {
				return err
			}
		case "assistant":
			if err := emitAssistant(ctx, conn, sid, env.Message, cwd, cache); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan history: %w", err)
	}
	return nil
}

// localCommandTagPattern matches the marker tags the CLI wraps around
// slash-command invocations and their captured output. The live prompt loop
// never surfaces these; replay must strip them too or the raw XML leaks into
// the client on session load.
// Go's RE2 engine has no backreferences, so each tag pair is spelled out.
var localCommandTagPattern = regexp.MustCompile(`(?s)<command-name>.*?</command-name>|<command-message>.*?</command-message>|<command-args>.*?</command-args>|<local-command-stdout>.*?</local-command-stdout>|<local-command-stderr>.*?</local-command-stderr>`)

// stripMarkerTags removes local-command marker tags, returning the remaining
// prose. ok is false when nothing renderable is left (e.g. a message that was
// purely a slash-command marker), signalling the caller to skip it.
func stripMarkerTags(text string) (string, bool) {
	stripped := localCommandTagPattern.ReplaceAllString(text, "")
	if strings.TrimSpace(stripped) == "" {
		return "", false
	}
	return stripped, true
}

// replayUserMessage emits either a user_message_chunk (for plain text turns)
// or one or more tool_call_update completions (for the user-role tool_result
// echoes the CLI writes after each tool returns).
func replayUserMessage(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage, cache toolUseCache) error {
	if len(raw) == 0 {
		return nil
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}

	// Plain-string content: a real user-typed prompt.
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		text, ok := stripMarkerTags(s)
		if !ok {
			return nil
		}
		return conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.UpdateUserMessageText(text),
		})
	}

	// Array content: text blocks become user message chunks; tool_result blocks
	// become tool_call completions. Everything else is dropped.
	var blocks []cliMsgBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text, ok := stripMarkerTags(b.Text)
			if !ok {
				continue
			}
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update:    acp.UpdateUserMessageText(text),
			}); err != nil {
				return err
			}
		case "tool_result":
			if b.ToolUseID == "" {
				continue
			}
			name := cache[b.ToolUseID]
			if isPlanTool(name) {
				continue
			}
			status := acp.ToolCallStatusCompleted
			if b.IsError {
				status = acp.ToolCallStatusFailed
			}
			opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}
			if content := toolResultContent(name, b); len(content) > 0 {
				opts = append(opts, acp.WithUpdateContent(content))
			}
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update:    acp.UpdateToolCall(acp.ToolCallId(b.ToolUseID), opts...),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
