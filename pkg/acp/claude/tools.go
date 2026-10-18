package claude

import (
	"cmp"
	"encoding/json"
	"regexp"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
)

type toolUseCache map[string]string

type toolInfo struct {
	title     string
	kind      acpsdk.ToolKind
	rawInput  any
	locations []acpsdk.ToolCallLocation
	content   []acpsdk.ToolCallContent
}

// toolCallStartUpdate builds the tool_call notification for a tool_use. It's
// shared by the two sites that can be first to surface one — the streamed
// tool_use block (events.go) and the eager permission-request path
// (approvals.go) — so they can't drift apart.
func toolCallStartUpdate(id, name string, rawInput json.RawMessage, cwd string, status acpsdk.ToolCallStatus) acpsdk.SessionUpdate {
	info := toolInfoFromToolUse(name, rawInput, cwd)
	opts := []acpsdk.ToolCallStartOpt{
		acpsdk.WithStartKind(info.kind),
		acpsdk.WithStartStatus(status),
	}
	if len(info.content) > 0 {
		opts = append(opts, acpsdk.WithStartContent(info.content))
	}
	if len(info.locations) > 0 {
		opts = append(opts, acpsdk.WithStartLocations(info.locations))
	}
	// WithStartLocations mirrors a lone location into rawInput.path. Apply the
	// display input afterwards so a file chip never becomes a duplicate hint or
	// expanded path argument.
	if info.rawInput != nil || len(info.locations) > 0 {
		opts = append(opts, acpsdk.WithStartRawInput(info.rawInput))
	}
	return acpsdk.StartToolCall(acpsdk.ToolCallId(id), info.title, opts...)
}

// toolCallRefineUpdate builds the tool_call_update that refines a tool_call
// already emitted by the other path with the now-complete info, instead of
// duplicating it. See toolCallTracker.
func toolCallRefineUpdate(id, name string, rawInput json.RawMessage, cwd string, status acpsdk.ToolCallStatus) acpsdk.SessionUpdate {
	info := toolInfoFromToolUse(name, rawInput, cwd)
	opts := []acpsdk.ToolCallUpdateOpt{
		acpsdk.WithUpdateTitle(info.title),
		acpsdk.WithUpdateKind(info.kind),
		acpsdk.WithUpdateStatus(status),
	}
	if info.rawInput != nil {
		opts = append(opts, acpsdk.WithUpdateRawInput(info.rawInput))
	}
	if len(info.content) > 0 {
		opts = append(opts, acpsdk.WithUpdateContent(info.content))
	}
	if len(info.locations) > 0 {
		opts = append(opts, acpsdk.WithUpdateLocations(info.locations))
	}
	return acpsdk.UpdateToolCall(acpsdk.ToolCallId(id), opts...)
}

func unmarshalAny(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

func displayInput(raw json.RawMessage, omit ...string) any {
	input, ok := unmarshalAny(raw)
	if !ok {
		return nil
	}
	args, ok := input.(map[string]any)
	if !ok {
		return input
	}
	for _, key := range omit {
		delete(args, key)
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

func displayLocation(path, cwd string, line int) []acpsdk.ToolCallLocation {
	if path == "" {
		return nil
	}
	location := acpsdk.ToolCallLocation{Path: acp.ResolvePath(path, cwd)}
	if line > 0 {
		location.Line = new(line)
	}
	return []acpsdk.ToolCallLocation{location}
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
		var content []acpsdk.ToolCallContent
		if in.Prompt != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(in.Prompt))}
		}
		return toolInfo{title: title, kind: acpsdk.ToolKindThink, content: content}

	case "Bash", "PowerShell":
		var in struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acpsdk.ToolCallContent
		if in.Description != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(in.Description))}
		}
		return toolInfo{
			title: "Run command", kind: acpsdk.ToolKindExecute,
			rawInput: displayInput(rawInput, "description"), content: content,
		}

	case "Read":
		var in struct {
			FilePath string `json:"file_path"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}
		_ = json.Unmarshal(rawInput, &in)
		return toolInfo{
			title: "Read file", kind: acpsdk.ToolKindRead,
			rawInput:  displayInput(rawInput, "file_path", "offset"),
			locations: displayLocation(in.FilePath, cwd, in.Offset),
		}

	case "Write":
		// The CLI also accepts path and file_text/file_content spellings.
		var in struct {
			FilePath    string `json:"file_path"`
			Path        string `json:"path"`
			Content     string `json:"content"`
			FileText    string `json:"file_text"`
			FileContent string `json:"file_content"`
		}
		_ = json.Unmarshal(rawInput, &in)
		in.FilePath = cmp.Or(in.FilePath, in.Path)
		in.Content = cmp.Or(in.Content, in.FileText, in.FileContent)
		var content []acpsdk.ToolCallContent
		var locations []acpsdk.ToolCallLocation
		if in.FilePath != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolDiffContent(in.FilePath, in.Content)}
			locations = displayLocation(in.FilePath, cwd, 0)
		} else if in.Content != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(in.Content))}
		}
		return toolInfo{title: "Write file", kind: acpsdk.ToolKindEdit, content: content, locations: locations}

	case "Edit":
		var in struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acpsdk.ToolCallContent
		var locations []acpsdk.ToolCallLocation
		var display any
		if in.FilePath != "" {
			if in.OldString != "" {
				content = []acpsdk.ToolCallContent{acpsdk.ToolDiffContent(in.FilePath, in.NewString, in.OldString)}
			} else if in.NewString != "" {
				content = []acpsdk.ToolCallContent{acpsdk.ToolDiffContent(in.FilePath, in.NewString)}
			}
			locations = displayLocation(in.FilePath, cwd, 0)
		}
		if len(content) > 0 {
			display = displayInput(rawInput, "file_path", "old_string", "new_string")
		} else {
			display = displayInput(rawInput, "file_path")
		}
		return toolInfo{title: "Edit file", kind: acpsdk.ToolKindEdit, rawInput: display, content: content, locations: locations}

	case "NotebookEdit":
		var in struct {
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(rawInput, &in)
		return toolInfo{
			title: "Edit notebook", kind: acpsdk.ToolKindEdit,
			rawInput:  displayInput(rawInput, "notebook_path"),
			locations: displayLocation(in.NotebookPath, cwd, 0),
		}

	case "Glob":
		var in struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal(rawInput, &in)
		return toolInfo{
			title: "Find files", kind: acpsdk.ToolKindSearch,
			rawInput:  displayInput(rawInput, "path"),
			locations: displayLocation(in.Path, cwd, 0),
		}

	case "Grep":
		var in struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(rawInput, &in)
		return toolInfo{
			title: "Search files", kind: acpsdk.ToolKindSearch,
			rawInput:  displayInput(rawInput, "path"),
			locations: displayLocation(in.Path, cwd, 0),
		}

	case "WebFetch":
		var in struct {
			URL    string `json:"url"`
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acpsdk.ToolCallContent
		if in.Prompt != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(in.Prompt))}
		}
		return toolInfo{
			title: "Open page", kind: acpsdk.ToolKindFetch,
			rawInput: displayInput(rawInput, "prompt"), content: content,
		}

	case "WebSearch":
		var in struct {
			Query          string   `json:"query"`
			AllowedDomains []string `json:"allowed_domains"`
			BlockedDomains []string `json:"blocked_domains"`
		}
		_ = json.Unmarshal(rawInput, &in)
		return toolInfo{title: "Web search", kind: acpsdk.ToolKindFetch, rawInput: displayInput(rawInput)}

	case "TaskOutput":
		return toolInfo{title: "Read command output", kind: acpsdk.ToolKindExecute, rawInput: displayInput(rawInput)}

	case "TaskStop":
		return toolInfo{title: "Stop command", kind: acpsdk.ToolKindExecute, rawInput: displayInput(rawInput)}

	case "EnterPlanMode":
		return toolInfo{title: "Planning", kind: acpsdk.ToolKindSwitchMode, rawInput: displayInput(rawInput)}

	case "ExitPlanMode":
		var in struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(rawInput, &in)
		var content []acpsdk.ToolCallContent
		if in.Plan != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(in.Plan))}
		}
		return toolInfo{title: "Ready to code?", kind: acpsdk.ToolKindSwitchMode, content: content}

	case "Skill":
		return toolInfo{title: "Load skill", kind: acpsdk.ToolKindOther, rawInput: displayInput(rawInput)}

	case "AskUserQuestion":
		return toolInfo{title: "Request input", kind: acpsdk.ToolKindOther}

	default:
		title := name
		if title == "" {
			title = "Tool call"
		}
		return toolInfo{title: title, kind: acpsdk.ToolKindOther, rawInput: displayInput(rawInput)}
	}
}

// shouldEagerlyEmitToolCall reports whether a permission request may publish
// the tool before its assistant tool_use arrives. Task/Agent wait for the
// assistant event so subagent calls never appear merely because a permission
// prompt raced ahead of the model output.
func shouldEagerlyEmitToolCall(name string) bool {
	return name != "Agent" && name != "Task"
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
