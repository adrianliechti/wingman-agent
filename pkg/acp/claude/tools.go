package claude

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coder/acp-go-sdk"
)

type toolUseCache map[string]string

type toolInfo struct {
	title     string
	kind      acp.ToolKind
	locations []acp.ToolCallLocation
	content   []acp.ToolCallContent
}

func toDisplayPath(filePath, cwd string) string {
	if cwd == "" || filePath == "" {
		return filePath
	}
	rc, err1 := filepath.Abs(cwd)
	rf, err2 := filepath.Abs(filePath)
	if err1 != nil || err2 != nil {
		return filePath
	}
	if rf == rc || strings.HasPrefix(rf, rc+string(filepath.Separator)) {
		if rel, err := filepath.Rel(rc, rf); err == nil {
			return rel
		}
	}
	return filePath
}

func toolInfoFromToolUse(name string, rawInput json.RawMessage, cwd string) toolInfo {
	switch name {
	case "Agent", "Task":
		var in struct {
			Description string `json:"description"`
			Prompt      string `json:"prompt"`
		}
		_ = json.Unmarshal(rawInput, &in)
		title := "Task"
		if in.Description != "" {
			title = in.Description
		}
		var content []acp.ToolCallContent
		if in.Prompt != "" {
			content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(in.Prompt))}
		}
		return toolInfo{title: title, kind: acp.ToolKindThink, content: content}

	case "Bash":
		var in struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(rawInput, &in)
		title := "Terminal"
		if in.Command != "" {
			title = in.Command
		}
		var content []acp.ToolCallContent
		if in.Description != "" {
			content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(in.Description))}
		}
		return toolInfo{title: title, kind: acp.ToolKindExecute, content: content}

	case "Read":
		var in struct {
			FilePath string `json:"file_path"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}
		_ = json.Unmarshal(rawInput, &in)
		span := ""
		switch {
		case in.Limit > 0:
			start := in.Offset
			if start == 0 {
				start = 1
			}
			span = fmt.Sprintf(" (%d - %d)", start, start+in.Limit-1)
		case in.Offset != 0:
			span = fmt.Sprintf(" (from line %d)", in.Offset)
		}
		display := "File"
		var locations []acp.ToolCallLocation
		if in.FilePath != "" {
			display = toDisplayPath(in.FilePath, cwd)
			line := in.Offset
			if line == 0 {
				line = 1
			}
			locations = []acp.ToolCallLocation{{Path: in.FilePath, Line: acp.Ptr(line)}}
		}
		return toolInfo{title: "Read " + display + span, kind: acp.ToolKindRead, locations: locations}

	case "Write":
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acp.ToolCallContent
		var locations []acp.ToolCallLocation
		title := "Write"
		if in.FilePath != "" {
			content = []acp.ToolCallContent{acp.ToolDiffContent(in.FilePath, in.Content)}
			locations = []acp.ToolCallLocation{{Path: in.FilePath}}
			title = "Write " + toDisplayPath(in.FilePath, cwd)
		} else if in.Content != "" {
			content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(in.Content))}
		}
		return toolInfo{title: title, kind: acp.ToolKindEdit, content: content, locations: locations}

	case "Edit", "MultiEdit":
		var in struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acp.ToolCallContent
		var locations []acp.ToolCallLocation
		title := "Edit"
		if in.FilePath != "" {
			if in.OldString != "" {
				content = []acp.ToolCallContent{acp.ToolDiffContent(in.FilePath, in.NewString, in.OldString)}
			} else if in.NewString != "" {
				content = []acp.ToolCallContent{acp.ToolDiffContent(in.FilePath, in.NewString)}
			}
			locations = []acp.ToolCallLocation{{Path: in.FilePath}}
			title = "Edit " + toDisplayPath(in.FilePath, cwd)
		}
		return toolInfo{title: title, kind: acp.ToolKindEdit, content: content, locations: locations}

	case "Glob":
		var in struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal(rawInput, &in)
		label := "Find"
		if in.Path != "" {
			label += " `" + in.Path + "`"
		}
		if in.Pattern != "" {
			label += " `" + in.Pattern + "`"
		}
		var locations []acp.ToolCallLocation
		if in.Path != "" {
			locations = []acp.ToolCallLocation{{Path: in.Path}}
		}
		return toolInfo{title: label, kind: acp.ToolKindSearch, locations: locations}

	case "Grep":
		return toolInfo{title: grepLabel(rawInput), kind: acp.ToolKindSearch}

	case "WebFetch":
		var in struct {
			URL    string `json:"url"`
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(rawInput, &in)
		title := "Fetch"
		if in.URL != "" {
			title = "Fetch " + in.URL
		}
		var content []acp.ToolCallContent
		if in.Prompt != "" {
			content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(in.Prompt))}
		}
		return toolInfo{title: title, kind: acp.ToolKindFetch, content: content}

	case "WebSearch":
		var in struct {
			Query          string   `json:"query"`
			AllowedDomains []string `json:"allowed_domains"`
			BlockedDomains []string `json:"blocked_domains"`
		}
		_ = json.Unmarshal(rawInput, &in)
		label := "Web search"
		if in.Query != "" {
			label = `"` + in.Query + `"`
		}
		if len(in.AllowedDomains) > 0 {
			label += " (allowed: " + strings.Join(in.AllowedDomains, ", ") + ")"
		}
		if len(in.BlockedDomains) > 0 {
			label += " (blocked: " + strings.Join(in.BlockedDomains, ", ") + ")"
		}
		return toolInfo{title: label, kind: acp.ToolKindFetch}

	case "ExitPlanMode":
		var in struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acp.ToolCallContent
		if in.Plan != "" {
			content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(in.Plan))}
		}
		return toolInfo{title: "Ready to code?", kind: acp.ToolKindSwitchMode, content: content}

	default:
		title := name
		if title == "" {
			title = "Tool call"
		}
		return toolInfo{title: title, kind: toolKindFor(name)}
	}
}

func grepLabel(rawInput json.RawMessage) string {
	var in struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		Type       string `json:"type"`
		OutputMode string `json:"output_mode"`
		I          bool   `json:"-i"`
		N          bool   `json:"-n"`
		A          *int   `json:"-A"`
		B          *int   `json:"-B"`
		C          *int   `json:"-C"`
		HeadLimit  *int   `json:"head_limit"`
		Multiline  bool   `json:"multiline"`
	}
	_ = json.Unmarshal(rawInput, &in)

	label := "grep"
	if in.I {
		label += " -i"
	}
	if in.N {
		label += " -n"
	}
	if in.A != nil {
		label += fmt.Sprintf(" -A %d", *in.A)
	}
	if in.B != nil {
		label += fmt.Sprintf(" -B %d", *in.B)
	}
	if in.C != nil {
		label += fmt.Sprintf(" -C %d", *in.C)
	}
	switch in.OutputMode {
	case "files_with_matches":
		label += " -l"
	case "count":
		label += " -c"
	}
	if in.HeadLimit != nil {
		label += fmt.Sprintf(" | head -%d", *in.HeadLimit)
	}
	if in.Glob != "" {
		label += fmt.Sprintf(` --include="%s"`, in.Glob)
	}
	if in.Type != "" {
		label += " --type=" + in.Type
	}
	if in.Multiline {
		label += " -P"
	}
	if in.Pattern != "" {
		label += fmt.Sprintf(` "%s"`, in.Pattern)
	}
	if in.Path != "" {
		label += " " + in.Path
	}
	return label
}

func isPlanTool(name string) bool { return name == "TodoWrite" }

func planEntriesFromTodoWrite(rawInput json.RawMessage) (entries []acp.PlanEntry, ok bool) {
	var in struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(rawInput, &in); err != nil || in.Todos == nil {
		return nil, false
	}
	entries = make([]acp.PlanEntry, 0, len(in.Todos))
	for _, t := range in.Todos {
		entries = append(entries, acp.PlanEntry{
			Content:  t.Content,
			Status:   planStatus(t.Status),
			Priority: acp.PlanEntryPriorityMedium,
		})
	}
	return entries, true
}

func planStatus(s string) acp.PlanEntryStatus {
	switch s {
	case "in_progress":
		return acp.PlanEntryStatusInProgress
	case "completed":
		return acp.PlanEntryStatusCompleted
	default:
		return acp.PlanEntryStatusPending
	}
}

var fencePattern = regexp.MustCompile("(?m)^`{3,}")

func markdownEscape(text string) string {
	fence := "```"
	for _, m := range fencePattern.FindAllString(text, -1) {
		for len(m) >= len(fence) {
			fence += "`"
		}
	}
	suffix := ""
	if !strings.HasSuffix(text, "\n") {
		suffix = "\n"
	}
	return fence + "\n" + text + suffix + fence
}
