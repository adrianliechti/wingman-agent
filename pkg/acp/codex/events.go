package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// eventDispatcher converts a stream of codex notifications into ACP session
// updates for one in-flight turn. The `done` channel fires on `turn/completed`
// so runTurn can unblock without a separate routing layer.
type eventDispatcher struct {
	ctx       context.Context
	conn      *acp.AgentSideConnection
	sessionID acp.SessionId
	done      chan turnCompleted

	mu      sync.Mutex
	failure error
}

func newEventDispatcher(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId) *eventDispatcher {
	return &eventDispatcher{
		ctx:       ctx,
		conn:      conn,
		sessionID: sid,
		done:      make(chan turnCompleted, 1),
	}
}

func (d *eventDispatcher) setFailure(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failure == nil {
		d.failure = err
	}
}

func (d *eventDispatcher) getFailure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failure
}

func isAuthError(info json.RawMessage) bool {
	if len(info) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(info, &s) == nil {
		return s == "unauthorized" || s == "usageLimitExceeded"
	}
	var obj map[string]struct {
		HTTPStatusCode int `json:"httpStatusCode"`
	}
	if json.Unmarshal(info, &obj) != nil {
		return false
	}
	for _, key := range []string{
		"httpConnectionFailed",
		"responseStreamConnectionFailed",
		"responseStreamDisconnected",
		"responseTooManyFailedAttempts",
	} {
		if v, ok := obj[key]; ok && v.HTTPStatusCode == 401 {
			return true
		}
	}
	return false
}

func (d *eventDispatcher) update(u acp.SessionUpdate) {
	if d.ctx.Err() != nil {
		return
	}
	_ = d.conn.SessionUpdate(d.ctx, acp.SessionNotification{
		SessionId: d.sessionID,
		Update:    u,
	})
}

func (d *eventDispatcher) handle(method string, params json.RawMessage) {
	switch method {
	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			d.update(acp.UpdateAgentMessageText(p.Delta))
		}

	case "item/started":
		d.handleItemStarted(params)

	case "item/completed":
		d.handleItemCompleted(params)

	case "turn/plan/updated":
		d.handlePlanUpdated(params)

	case "error":
		var p struct {
			Error struct {
				Message        string          `json:"message"`
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		if isAuthError(p.Error.CodexErrorInfo) {
			d.setFailure(acp.NewAuthRequired(nil))
		}
		if p.Error.Message != "" {
			d.update(acp.UpdateAgentMessageText(p.Error.Message + "\n\n"))
		}

	case "warning":
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(params, &p) == nil && p.Message != "" {
			d.update(acp.UpdateAgentMessageText("Warning: " + p.Message + "\n\n"))
		}

	case "item/commandExecution/outputDelta":
		// Stream stdout chunks as plain content; the SDK's tool-call helpers
		// don't yet expose a terminal_output_delta meta block.
		var p struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			d.update(acp.UpdateToolCall(acp.ToolCallId(p.ItemID), acp.WithUpdateContent([]acp.ToolCallContent{
				acp.ToolContent(acp.TextBlock(p.Delta)),
			})))
		}

	case "turn/completed":
		var tc turnCompleted
		if json.Unmarshal(params, &tc) == nil {
			select {
			case d.done <- tc:
			default:
			}
		}
	}
}

func (d *eventDispatcher) handleItemStarted(params json.RawMessage) {
	var env struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		return
	}
	id, kind, ok := peekItem(env.Item)
	if !ok {
		return
	}

	switch kind {
	case "commandExecution":
		var it struct {
			Command        string          `json:"command"`
			Cwd            string          `json:"cwd"`
			CommandActions []commandAction `json:"commandActions"`
		}
		_ = json.Unmarshal(env.Item, &it)
		if title, kind, locs, ok := commandActionToolCall(it.CommandActions); ok {
			opts := []acp.ToolCallStartOpt{
				acp.WithStartKind(kind),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
			}
			if len(locs) > 0 {
				opts = append(opts, acp.WithStartLocations(locs))
			}
			d.update(acp.StartToolCall(acp.ToolCallId(id), title, opts...))
			break
		}
		title := stripShellPrefix(it.Command)
		if title == "" {
			title = "Run command"
		}
		d.update(acp.StartToolCall(acp.ToolCallId(id), title,
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartRawInput(map[string]any{"command": it.Command, "cwd": it.Cwd}),
		))

	case "fileChange":
		opts := []acp.ToolCallStartOpt{
			acp.WithStartKind(acp.ToolKindEdit),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
		}
		if content := fileChangeContent(env.Item); len(content) > 0 {
			opts = append(opts, acp.WithStartContent(content))
		}
		d.update(acp.StartToolCall(acp.ToolCallId(id), "Editing files", opts...))

	case "mcpToolCall":
		var it struct {
			Server string          `json:"server"`
			Tool   string          `json:"tool"`
			Args   json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(env.Item, &it)
		var args map[string]any
		_ = json.Unmarshal(it.Args, &args)
		d.update(acp.StartToolCall(acp.ToolCallId(id), fmt.Sprintf("mcp.%s.%s", it.Server, it.Tool),
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartRawInput(map[string]any{"server": it.Server, "tool": it.Tool, "arguments": args}),
		))

	case "dynamicToolCall":
		var it struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(env.Item, &it)
		var args map[string]any
		_ = json.Unmarshal(it.Args, &args)
		d.update(acp.StartToolCall(acp.ToolCallId(id), it.Tool,
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartRawInput(map[string]any{"arguments": args}),
		))

	case "webSearch":
		var it struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(env.Item, &it)
		title := "Web search"
		if it.Query != "" {
			title = "Web search: " + it.Query
		}
		d.update(acp.StartToolCall(acp.ToolCallId(id), title,
			acp.WithStartKind(acp.ToolKindSearch),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
		))
	}
}

func (d *eventDispatcher) handleItemCompleted(params json.RawMessage) {
	var env struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		return
	}
	id, kind, ok := peekItem(env.Item)
	if !ok {
		return
	}

	switch kind {
	case "commandExecution":
		var it struct {
			Status           string  `json:"status"`
			AggregatedOutput *string `json:"aggregatedOutput"`
		}
		_ = json.Unmarshal(env.Item, &it)
		opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(toolStatusFor(it.Status))}
		if it.AggregatedOutput != nil && *it.AggregatedOutput != "" {
			opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{
				acp.ToolContent(acp.TextBlock(*it.AggregatedOutput)),
			}))
		}
		d.update(acp.UpdateToolCall(acp.ToolCallId(id), opts...))

	case "fileChange":
		var it struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(env.Item, &it)
		opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(toolStatusFor(it.Status))}
		if content := fileChangeContent(env.Item); len(content) > 0 {
			opts = append(opts, acp.WithUpdateContent(content))
		}
		d.update(acp.UpdateToolCall(acp.ToolCallId(id), opts...))

	case "dynamicToolCall", "mcpToolCall", "webSearch":
		var it struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(env.Item, &it)
		status := toolStatusFor(it.Status)
		if kind == "webSearch" {
			status = acp.ToolCallStatusCompleted
		}
		d.update(acp.UpdateToolCall(acp.ToolCallId(id), acp.WithUpdateStatus(status)))

	case "reasoning":
		var it struct {
			Summary []string `json:"summary"`
		}
		_ = json.Unmarshal(env.Item, &it)
		if len(it.Summary) > 0 && it.Summary[0] != "" {
			d.update(acp.UpdateAgentThoughtText(it.Summary[0]))
		}
	}
}

func (d *eventDispatcher) handlePlanUpdated(params json.RawMessage) {
	var p struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	entries := make([]acp.PlanEntry, 0, len(p.Plan))
	for _, e := range p.Plan {
		status := acp.PlanEntryStatusPending
		switch e.Status {
		case "inProgress":
			status = acp.PlanEntryStatusInProgress
		case "completed":
			status = acp.PlanEntryStatusCompleted
		}
		entries = append(entries, acp.PlanEntry{
			Content:  e.Step,
			Priority: acp.PlanEntryPriorityMedium,
			Status:   status,
		})
	}
	d.update(acp.UpdatePlan(entries...))
}

func peekItem(item json.RawMessage) (id, kind string, ok bool) {
	var probe struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(item, &probe); err != nil {
		return "", "", false
	}
	return probe.ID, probe.Type, probe.ID != "" && probe.Type != ""
}

func toolStatusFor(s string) acp.ToolCallStatus {
	switch s {
	case "inProgress":
		return acp.ToolCallStatusInProgress
	case "failed", "declined":
		return acp.ToolCallStatusFailed
	default:
		return acp.ToolCallStatusCompleted
	}
}

// commandAction describes a structured interpretation of a commandExecution
// item (read/search/listFiles) that codex attaches alongside the raw shell
// command.
type commandAction struct {
	Type  string `json:"type"`
	Path  string `json:"path"`
	Query string `json:"query"`
}

// commandActionToolCall maps a commandExecution that resolves to a single
// structured action onto a descriptive title, tool kind, and locations, so the
// call renders as e.g. "Read file '...'" or "Search for '...'" instead of the
// raw shell command. ok is false when there is no single mappable action and
// the caller should fall back to the plain command title.
func commandActionToolCall(actions []commandAction) (title string, kind acp.ToolKind, locations []acp.ToolCallLocation, ok bool) {
	if len(actions) != 1 {
		return "", "", nil, false
	}
	a := actions[0]
	switch a.Type {
	case "read":
		if a.Path == "" {
			return "", "", nil, false
		}
		return fmt.Sprintf("Read file '%s'", a.Path), acp.ToolKindRead, []acp.ToolCallLocation{{Path: a.Path}}, true
	case "search":
		return searchTitle(a.Query, a.Path), acp.ToolKindSearch, nil, true
	case "listFiles":
		if a.Path != "" {
			return fmt.Sprintf("List files in '%s'", a.Path), acp.ToolKindRead, nil, true
		}
		return "List files", acp.ToolKindRead, nil, true
	}
	return "", "", nil, false
}

func searchTitle(query, path string) string {
	switch {
	case query != "" && path != "":
		return fmt.Sprintf("Search for '%s' in %s", query, path)
	case query != "":
		return fmt.Sprintf("Search for '%s'", query)
	case path != "":
		return fmt.Sprintf("Search in '%s'", path)
	default:
		return "Search"
	}
}

// fileChange describes one file modification carried by a fileChange item.
type fileChange struct {
	Path string `json:"path"`
	Kind struct {
		Type string `json:"type"`
	} `json:"kind"`
	Diff string `json:"diff"`
}

// fileChangeContent converts the changes of a fileChange item into ACP diff
// content blocks so the edit renders as a before/after diff in the client
// instead of an empty tool call. Old and new text are reconstructed directly
// from the unified diff each change carries, which avoids depending on the file
// contents on disk or on the timing of codex's apply step.
func fileChangeContent(raw json.RawMessage) []acp.ToolCallContent {
	var it struct {
		Changes []fileChange `json:"changes"`
	}
	if err := json.Unmarshal(raw, &it); err != nil {
		return nil
	}
	var content []acp.ToolCallContent
	for _, ch := range it.Changes {
		if ch.Path == "" {
			continue
		}
		var oldText *string
		var newText string
		if ch.Kind.Type == "add" && !isUnifiedDiff(ch.Diff) {
			// New files may carry raw contents instead of a patch.
			newText = ch.Diff
		} else {
			old, nw := splitUnifiedDiff(ch.Diff)
			newText = nw
			if ch.Kind.Type != "add" {
				oldText = &old
			}
		}
		content = append(content, acp.ToolCallContent{
			Diff: &acp.ToolCallContentDiff{
				Type:    "diff",
				Path:    ch.Path,
				OldText: oldText,
				NewText: newText,
				// Surface the change kind so the client can distinguish a
				// deletion (newText must be nulled) from an edit-to-empty.
				Meta: map[string]any{"kind": ch.Kind.Type},
			},
		})
	}
	return content
}

func isUnifiedDiff(s string) bool {
	return strings.HasPrefix(s, "--- ") || strings.Contains(s, "\n--- ")
}

// splitUnifiedDiff reconstructs the old and new text from a unified diff by
// walking its hunks: context lines belong to both sides, "-" lines to the old
// text only, and "+" lines to the new text only.
func splitUnifiedDiff(diff string) (oldText, newText string) {
	var oldLines, newLines []string
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
			// skip file headers (--- / +++) and any preamble
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" marker
		case strings.HasPrefix(line, "-"):
			oldLines = append(oldLines, line[1:])
		case strings.HasPrefix(line, "+"):
			newLines = append(newLines, line[1:])
		default:
			// context line (" prefix") or a bare empty context line
			text := line
			if strings.HasPrefix(line, " ") {
				text = line[1:]
			}
			oldLines = append(oldLines, text)
			newLines = append(newLines, text)
		}
	}
	return strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
}

// shellPrefixRe matches a leading shell invocation such as
// "bash -lc ", "/bin/zsh -c ", or "sh ", so the underlying command can be
// shown on its own.
var shellPrefixRe = regexp.MustCompile(`^(?:/bin/)?(?:bash|zsh|sh)\s+(?:-[lc]+\s+)?`)

// stripShellPrefix trims a leading shell invocation so tool-call titles show
// the actual command. Mirrors the TS reference's CommandUtils.
func stripShellPrefix(cmd string) string {
	c := strings.TrimSpace(cmd)
	c = shellPrefixRe.ReplaceAllString(c, "")
	// Strip a single pair of surrounding single quotes, if present.
	if len(c) >= 2 && strings.HasPrefix(c, "'") && strings.HasSuffix(c, "'") {
		c = c[1 : len(c)-1]
	}
	return c
}
