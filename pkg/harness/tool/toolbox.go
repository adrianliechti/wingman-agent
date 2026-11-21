package tool

import "context"

// Toolbox lets a running Composite tool call the agent's other tools. Each
// call gets the hooks and policy a call from the model gets.
type Toolbox interface {
	// Tools lists the other tools the model can call this turn.
	Tools() []Tool
	// Call runs one of them. A failure is a Result with IsError set.
	Call(ctx context.Context, name string, args map[string]any) Result
}

type toolboxKey struct{}

func WithToolbox(ctx context.Context, toolbox Toolbox) context.Context {
	return context.WithValue(ctx, toolboxKey{}, toolbox)
}

// ToolboxFromContext returns nil unless ctx is that of a Composite tool the
// model called itself.
func ToolboxFromContext(ctx context.Context) Toolbox {
	toolbox, _ := ctx.Value(toolboxKey{}).(Toolbox)
	return toolbox
}
