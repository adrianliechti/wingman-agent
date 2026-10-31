package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/pathutil"
)

const (
	headBytes = 64 * 1024
	tailBytes = 256 * 1024
)

type sessionFileInfo struct {
	ID        string
	Cwd       string
	Title     string
	UpdatedAt string
	Path      string
}

func listSessionFiles(sessionsDir string) []sessionFileInfo {
	out := scanSessionHeaders(sessionsDir)
	for i := range out {
		info := &out[i]
		tail, whole := readTail(info.Path, tailBytes)
		info.Title = sessionName(info.Path, tail, whole)
		info.UpdatedAt = pickUpdatedAtFromTail(tail)

		if info.UpdatedAt == "" {
			if st, err := os.Stat(info.Path); err == nil {
				info.UpdatedAt = st.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00")
			}
		}
		if info.Title == "" {
			info.Title = pickFirstUserMessage(info.Path)
		}
	}

	slices.SortStableFunc(out, func(a, b sessionFileInfo) int {
		return strings.Compare(b.UpdatedAt, a.UpdatedAt)
	})
	return out
}

func scanSessionHeaders(sessionsDir string) []sessionFileInfo {
	var out []sessionFileInfo
	_ = pathutil.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if id, cwd, ok := parseSessionHeader(readFirstLine(path)); ok {
			out = append(out, sessionFileInfo{ID: id, Cwd: cwd, Path: path})
		}
		return nil
	})
	return out
}

func canonicalPath(path string) string {
	if resolved, err := pathutil.Resolve(path); err == nil {
		return resolved
	}
	return path
}

func findSessionFile(sessionsDir, sessionID string) (sessionFileInfo, bool) {
	for _, s := range scanSessionHeaders(sessionsDir) {
		if s.ID == sessionID {
			return s, true
		}
	}
	return sessionFileInfo{}, false
}

func readFirstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, headBytes)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

// readTail also reports whether the tail covers the whole file.
func readTail(path string, n int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", true
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", true
	}
	start := max(st.Size()-n, 0)
	if _, err := f.Seek(start, 0); err != nil {
		return "", true
	}
	data, _ := io.ReadAll(f)
	return string(data), start == 0
}

func parseSessionHeader(line string) (id, cwd string, ok bool) {
	if line == "" {
		return "", "", false
	}
	var h struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Cwd  string `json:"cwd"`
	}
	if json.Unmarshal([]byte(line), &h) != nil || h.Type != "session" {
		return "", "", false
	}
	if h.ID == "" || h.Cwd == "" {
		return "", "", false
	}
	return h.ID, h.Cwd, true
}

func readSessionTitle(path string) (name, prompt string) {
	tail, whole := readTail(path, tailBytes)
	return sessionName(path, tail, whole), pickFirstUserMessage(path)
}

// sessionName follows Pi: the latest session_info entry wins, and an empty name clears it.
func sessionName(path, tail string, whole bool) string {
	if name, ok := pickNameFromTail(tail); ok || whole {
		return name
	}
	return scanSessionName(path)
}

func pickNameFromTail(tail string) (string, bool) {
	lines := strings.Split(tail, "\n")
	for _, line := range slices.Backward(lines) {
		if name, ok := parseSessionName([]byte(line)); ok {
			return name, true
		}
	}
	return "", false
}

func scanSessionName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	name := ""
	r := bufio.NewReaderSize(f, headBytes)
	for {
		line, err := r.ReadSlice('\n')
		oversized := false
		for errors.Is(err, bufio.ErrBufferFull) {
			// Oversized lines hold message payloads, never a session name.
			oversized = true
			_, err = r.ReadSlice('\n')
		}
		if !oversized {
			if n, ok := parseSessionName(line); ok {
				name = n
			}
		}
		if err != nil {
			return name
		}
	}
}

func parseSessionName(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte(`"session_info"`)) {
		return "", false
	}
	var obj struct {
		Type string  `json:"type"`
		Name *string `json:"name"`
	}
	if json.Unmarshal(line, &obj) != nil || obj.Type != "session_info" || obj.Name == nil {
		return "", false
	}
	return strings.TrimSpace(*obj.Name), true
}

func pickUpdatedAtFromTail(tail string) string {
	lines := strings.Split(tail, "\n")
	for _, line := range slices.Backward(lines) {
		line := strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &obj) == nil && obj.Type == "message" && obj.Timestamp != "" {
			return obj.Timestamp
		}
	}
	return ""
}

func pickFirstUserMessage(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	count := 0
	for scanner.Scan() {
		count++
		if count > 2000 {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &obj) != nil || obj.Type != "message" || obj.Message.Role != "user" {
			continue
		}
		if title := sessionTitleText(piMessageTexts(obj.Message.Content)); title != "" {
			return title
		}
	}
	return ""
}

const maxTitleRunes = 80

func sessionTitleText(texts []string) string {
	title := strings.Join(strings.Fields(strings.Join(texts, " ")), " ")
	if runes := []rune(title); len(runes) > maxTitleRunes {
		title = string(runes[:maxTitleRunes])
	}
	return title
}

func piMessageTexts(content json.RawMessage) []string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []string{s}
	}
	var texts []string
	for _, block := range parsePiMessageContent(content) {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	return texts
}

func replayMessages(send func(acp.SessionUpdate), data json.RawMessage, cwd string) {
	var d struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}

	startedTools := make(map[string]bool)
	for _, raw := range d.Messages {
		var m struct {
			Role       string          `json:"role"`
			Timestamp  int64           `json:"timestamp"`
			Content    json.RawMessage `json:"content"`
			ToolName   string          `json:"toolName"`
			ToolCallID string          `json:"toolCallId"`
			IsError    bool            `json:"isError"`
			Args       json.RawMessage `json:"args"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}

		messageID := piMessageID(m.Role, m.Timestamp)
		message := func(update acp.SessionUpdate) {
			if messageID != "" {
				switch {
				case update.UserMessageChunk != nil:
					update.UserMessageChunk.MessageId = new(messageID)
				case update.AgentMessageChunk != nil:
					update.AgentMessageChunk.MessageId = new(messageID)
				case update.AgentThoughtChunk != nil:
					update.AgentThoughtChunk.MessageId = new(messageID + "-thought")
				}
			}
			send(update)
		}
		switch m.Role {
		case "user":
			var text string
			if json.Unmarshal(m.Content, &text) == nil {
				if text != "" {
					message(acp.UpdateUserMessage(acp.TextBlock(text)))
				}
				continue
			}
			for _, block := range parsePiMessageContent(m.Content) {
				switch block.Type {
				case "text":
					if block.Text != "" {
						message(acp.UpdateUserMessage(acp.TextBlock(block.Text)))
					}
				case "image":
					if block.Data != "" {
						message(acp.UpdateUserMessage(acp.ImageBlock(block.Data, block.MimeType)))
					}
				}
			}

		case "assistant":
			for _, block := range parsePiMessageContent(m.Content) {
				switch block.Type {
				case "text":
					if block.Text != "" {
						message(acp.UpdateAgentMessageText(block.Text))
					}
				case "thinking":
					if block.Thinking != "" {
						message(acp.UpdateAgentThoughtText(block.Thinking))
					}
				case "toolCall":
					if block.ID == "" {
						continue
					}
					startedTools[block.ID] = true
					presentation := presentTool(block.Name, block.Arguments, cwd)
					send(acp.StartToolCall(
						acp.ToolCallId(block.ID), presentation.title,
						startToolOptions(presentation, acp.ToolCallStatusPending)...,
					))
				}
			}

		case "toolResult":
			id := m.ToolCallID
			if id == "" {
				id = uuid.NewString()
			}
			name := m.ToolName
			status := acp.ToolCallStatusCompleted
			if m.IsError {
				status = acp.ToolCallStatusFailed
			}
			var args map[string]any
			_ = json.Unmarshal(m.Args, &args)
			if !startedTools[id] {
				presentation := presentTool(name, args, cwd)
				send(acp.StartToolCall(
					acp.ToolCallId(id), presentation.title,
					startToolOptions(presentation, status)...,
				))
			}
			opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}
			if content := piToolResultContent(m.Content); len(content) > 0 {
				opts = append(opts, acp.WithUpdateContent(content))
			}
			send(acp.UpdateToolCall(acp.ToolCallId(id), opts...))
			delete(startedTools, id)
		}
	}
}

type piMessageContent struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Thinking  string         `json:"thinking"`
	Data      string         `json:"data"`
	MimeType  string         `json:"mimeType"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func parsePiMessageContent(content json.RawMessage) []piMessageContent {
	var blocks []piMessageContent
	_ = json.Unmarshal(content, &blocks)
	return blocks
}

func piToolResultContent(content json.RawMessage) []acp.ToolCallContent {
	var out []acp.ToolCallContent
	for _, block := range parsePiMessageContent(content) {
		switch block.Type {
		case "text":
			if block.Text != "" {
				out = append(out, acp.ToolContent(acp.TextBlock(block.Text)))
			}
		case "image":
			if block.Data != "" {
				out = append(out, acp.ToolContent(acp.ImageBlock(block.Data, block.MimeType)))
			}
		}
	}
	return out
}

// Pi persists message timestamps and includes them in message_start events.
func piMessageID(role string, timestamp int64) string {
	if timestamp <= 0 {
		return ""
	}
	return role + "-" + strconv.FormatInt(timestamp, 10)
}
