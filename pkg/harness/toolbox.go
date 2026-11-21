package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type nestedCallKey struct{}

// toolbox runs the agent's other tools on behalf of one running tool call.
// Nested calls pass the tool hooks and the mode policy but are not recorded
// in the conversation: only the calling tool's result reaches the model.
type toolbox struct {
	agent  *Agent
	parent ToolCall
	tools  []tool.Tool
	calls  atomic.Int64

	mu      sync.Mutex
	results []NestedToolResult
}

// NestedResultsMetadata lists, on a composite tool's result, the calls it
// made, so the turn review and undo account for their edits and checks.
const NestedResultsMetadata = "nested_tool_results"

// NestedToolResult is what a turn review reads of a nested call. Arguments
// and metadata are kept only for the tools a review inspects.
type NestedToolResult struct {
	Name     string         `json:"name"`
	Args     string         `json:"args,omitempty"`
	IsError  bool           `json:"isError,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (b *toolbox) record(call ToolCall, result tool.Result) {
	nested := NestedToolResult{Name: call.Name, IsError: result.IsError}
	switch call.Name {
	case "exec", "exec_command", "exec_session":
		nested.Args, nested.Metadata = call.Args, maps.Clone(result.Metadata)
	case "edit", "agent":
		nested.Metadata = maps.Clone(result.Metadata)
	}
	b.mu.Lock()
	b.results = append(b.results, nested)
	b.mu.Unlock()
}

// nested returns the calls made so far, in the order they finished.
func (b *toolbox) nested() []NestedToolResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]NestedToolResult{}, b.results...)
}

func (b *toolbox) Tools() []tool.Tool {
	return slices.DeleteFunc(slices.Clone(b.tools), func(t tool.Tool) bool { return t.Name == b.parent.Name || t.Interactive })
}

func (b *toolbox) Call(ctx context.Context, name string, args map[string]any) tool.Result {
	if name == b.parent.Name {
		return tool.Error(fmt.Sprintf("error: %s cannot call itself", name))
	}
	if t := findTool(name, b.tools); t != nil && t.Interactive {
		return tool.Error(fmt.Sprintf("error: %s can only be called directly", name))
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return tool.Error(fmt.Sprintf("error: invalid arguments for %s: %v", name, err))
	}
	call := ToolCall{ID: fmt.Sprintf("%s.%d", b.parent.ID, b.calls.Add(1)), Name: name, Args: string(raw)}

	request := toolTelemetryRequest(call, b.tools)
	ctx, execution := b.agent.Telemetry.StartTool(ctx, request)
	result := b.agent.executeSingleToolCall(context.WithValue(ctx, nestedCallKey{}, true), call, b.tools)
	if request.MCPMethod == "" {
		execution.SetResult(genericToolResultTelemetryValue(result.Content.Text()))
	}
	execution.End(toolTelemetryOutcome(result))
	b.record(call, result)
	return result
}
