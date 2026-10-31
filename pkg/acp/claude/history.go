package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

const maxProjectKeyLen = 200

func projectsRoot(env []string) string {
	dir := claudeConfigDir(env)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "projects")
}

// encodeCwd matches Claude Code, which replaces every UTF-16 unit that is not ASCII alphanumeric.
func encodeCwd(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func projectDirsFor(cwd string, env []string) []string {
	root := projectsRoot(env)
	if cwd == "" || root == "" {
		return nil
	}
	key := encodeCwd(resolveProjectCwd(cwd))
	if len(key) <= maxProjectKeyLen {
		return []string{filepath.Join(root, key)}
	}
	// Longer names have runtime-specific hash suffixes. These candidates need a transcript cwd check.
	prefix := key[:maxProjectKeyLen] + "-"
	entries, _ := os.ReadDir(root)
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}

func resolveProjectCwd(cwd string) string {
	if cwd != "" {
		if resolved, err := pathutil.Resolve(cwd); err == nil {
			return resolved
		}
	}
	return cwd
}

func matchesProjectCwd(cwd, recordedCwd string) bool {
	if cwd == "" {
		return true
	}
	if recordedCwd == "" {
		// A truncated prefix cannot identify the project without its recorded cwd.
		return len(encodeCwd(cwd)) <= maxProjectKeyLen
	}
	return cwd == resolveProjectCwd(recordedCwd)
}

func transcriptPath(cwd, id string, env []string) string {
	cwd = resolveProjectCwd(cwd)
	for _, dir := range projectDirsFor(cwd, env) {
		path := filepath.Join(dir, id+".jsonl")
		if _, err := os.Stat(path); err == nil {
			if len(encodeCwd(cwd)) > maxProjectKeyLen && !matchesProjectCwd(cwd, scanSessionCwd(path)) {
				continue
			}
			return path
		}
	}
	return ""
}

type historyHeader struct {
	Type            string               `json:"type"`
	AITitle         string               `json:"aiTitle,omitempty"`
	Cwd             string               `json:"cwd,omitempty"`
	ParentToolUseID string               `json:"parent_tool_use_id,omitempty"`
	ParentAgentID   string               `json:"parent_agent_id,omitempty"`
	Message         historyHeaderMessage `json:"message"`
}

type historyHeaderMessage struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Model   string          `json:"model,omitempty"`
}

func scanSessionModel(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var model string
	scanner := newCLIScanner(f)
	for scanner.Scan() {
		var h historyHeader
		if json.Unmarshal(scanner.Bytes(), &h) == nil && h.Type == "assistant" && h.ParentToolUseID == "" && h.ParentAgentID == "" {
			if current := realModelID(h.Message.Model); current != "" {
				model = current
			}
		}
	}
	_ = scanner.Err()
	return model
}

func listProjectSessions(cwd string, env []string) ([]acpsdk.SessionInfo, error) {
	cwd = resolveProjectCwd(cwd)
	dirs, err := projectDirs(cwd, env)
	if err != nil {
		return nil, err
	}
	out := make([]acpsdk.SessionInfo, 0)
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
			if !matchesProjectCwd(cwd, recordedCwd) {
				continue
			}
			sessCwd := recordedCwd
			if sessCwd == "" {
				sessCwd = cwd
			}
			if sessCwd == "" {

				continue
			}
			ts := info.ModTime().UTC().Format(time.RFC3339)
			si := acpsdk.SessionInfo{
				SessionId: acpsdk.SessionId(id),
				Cwd:       sessCwd,
				UpdatedAt: &ts,
			}
			if title != "" {
				si.Title = &title
			}
			out = append(out, si)
		}
	}
	slices.SortFunc(out, func(a, b acpsdk.SessionInfo) int {
		return strings.Compare(*b.UpdatedAt, *a.UpdatedAt)
	})
	return out, nil
}

func projectDirs(cwd string, env []string) ([]string, error) {
	if cwd != "" {
		return projectDirsFor(cwd, env), nil
	}
	root := projectsRoot(env)
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

func deleteProjectSession(id acpsdk.SessionId, env []string) error {
	// Claude Code names transcripts by UUID; any other id must not reach the filesystem.
	if uuid.Validate(string(id)) != nil {
		return nil
	}
	dirs, err := projectDirs("", env)
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

func sessionExists(cwd string, id acpsdk.SessionId, env []string) bool {
	return transcriptPath(cwd, string(id), env) != ""
}

func scanSessionCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := newCLIScanner(f)
	for scanner.Scan() {
		var h struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(scanner.Bytes(), &h) == nil && h.Cwd != "" {
			return h.Cwd
		}
	}
	return ""
}

func scanSessionMetadata(path string) (title, cwd string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scanner := newCLIScanner(f)

	var firstUserText, latestAITitle string
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
			// The CLI appends a fresh ai-title line as the conversation
			// evolves rather than rewriting the old one, so keep scanning
			// and remember the latest rather than returning the first.
			if h.AITitle != "" {
				latestAITitle = h.AITitle
			}
		case "user":
			if firstUserText == "" {
				if text := firstTextFromContent(h.Message.Content); text != "" {
					firstUserText = text
				}
			}
		}
	}
	_ = scanner.Err()
	if latestAITitle != "" {
		return latestAITitle, cwd
	}
	if firstUserText != "" {
		return truncateTitle(firstUserText), cwd
	}
	return "", cwd
}

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

func replayHistory(ctx context.Context, conn acp.ClientConnection, sid acpsdk.SessionId, cwd, transcriptID string, env []string) error {
	path := transcriptPath(cwd, transcriptID, env)
	if path == "" {
		return nil
	}
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

func streamHistory(ctx context.Context, conn acp.ClientConnection, sid acpsdk.SessionId, cwd string, r io.Reader) error {
	cache := toolUseCache{}
	scanner := newCLIScanner(r)
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
			if err := replayUserMessage(ctx, conn, sid, env.Message, cache, env.ParentToolUseID, env.UUID); err != nil {
				return err
			}
		case "assistant":
			var message cliMessage
			if env.ParentToolUseID == "" && json.Unmarshal(env.Message, &message) == nil && isSyntheticLoginMessage(message) {
				continue
			}
			if err := emitAssistant(ctx, conn, sid, env.Message, cwd, cache, nil, nil, env.ParentToolUseID); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan history: %w", err)
	}
	return nil
}

var localCommandStdoutPattern = regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`)

var localCommandTagPattern = regexp.MustCompile(`(?s)<command-name>.*?</command-name>|<command-message>.*?</command-message>|<command-args>.*?</command-args>|<local-command-stderr>.*?</local-command-stderr>|<system-reminder>.*?</system-reminder>`)

var (
	commandNamePattern = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgsPattern = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// replayHiddenCommands are client-local commands whose marker-only records are UI noise, not prompts.
var replayHiddenCommands = map[string]bool{
	"/context": true, "/heapdump": true, "/extra-usage": true,
	"/compact": true, "/model": true, "/status": true, "/usage": true,
}

func stripMarkerTags(text string) (string, bool) {
	stripped := localCommandStdoutPattern.ReplaceAllString(text, "$1")
	stripped = localCommandTagPattern.ReplaceAllString(stripped, "")
	if strings.TrimSpace(stripped) == "" {
		return "", false
	}
	return stripped, true
}

// A marker-only slash skill invocation is rebuilt as the command the user typed.
func replayUserText(text string) (string, bool) {
	if stripped, ok := stripMarkerTags(text); ok {
		return stripped, true
	}
	if strings.Contains(text, "<local-command-stdout>") || strings.Contains(text, "<local-command-stderr>") {
		return "", false
	}
	m := commandNamePattern.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	name := strings.TrimSpace(m[1])
	if !strings.HasPrefix(name, "/") || replayHiddenCommands[strings.SplitN(name, " ", 2)[0]] {
		return "", false
	}
	if args := commandArgsPattern.FindStringSubmatch(text); args != nil && strings.TrimSpace(args[1]) != "" {
		return name + " " + strings.TrimSpace(args[1]), true
	}
	return name, true
}

func replayUserMessage(ctx context.Context, conn acp.ClientConnection, sid acpsdk.SessionId, raw json.RawMessage, cache toolUseCache, parentToolUseID, messageID string) error {
	if len(raw) == 0 {
		return nil
	}
	userText := func(text string) acpsdk.SessionUpdate {
		update := acpsdk.UpdateUserMessageText(text)
		if messageID != "" {
			update.UserMessageChunk.MessageId = new(messageID)
		}
		return update
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}

	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		if parentToolUseID != "" {
			return nil
		}
		text, ok := replayUserText(s)
		if !ok {
			return nil
		}
		return acp.Notify(ctx, conn, sid, userText(text))
	}

	var blocks []cliMsgBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if parentToolUseID != "" {
				continue
			}
			text, ok := replayUserText(b.Text)
			if !ok {
				continue
			}
			if err := acp.Notify(ctx, conn, sid, userText(text)); err != nil {
				return err
			}
		case "tool_result":
			if b.ToolUseID == "" {
				continue
			}
			name := cache[b.ToolUseID]
			status := acpsdk.ToolCallStatusCompleted
			if b.IsError {
				status = acpsdk.ToolCallStatusFailed
			}
			opts := []acpsdk.ToolCallUpdateOpt{acpsdk.WithUpdateStatus(status)}
			if content := toolResultContent(name, b); len(content) > 0 {
				opts = append(opts, acpsdk.WithUpdateContent(content))
			}
			u := acpsdk.UpdateToolCall(acpsdk.ToolCallId(b.ToolUseID), opts...)
			withClaudeToolMeta(&u, name, parentToolUseID)
			if err := acp.Notify(ctx, conn, sid, u); err != nil {
				return err
			}
		}
	}
	return nil
}
