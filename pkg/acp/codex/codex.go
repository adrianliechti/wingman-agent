package codex

import (
	"context"
	"encoding/json"
	"sync"
)

// codexClient is a thin typed wrapper around rpcClient that exposes the subset
// of `codex app-server` methods this agent uses. Notifications and inbound
// approval requests are routed by `threadId` to per-session handlers.
type codexClient struct {
	rpc *rpcClient

	mu       sync.Mutex
	handlers map[string]*threadHandlers
}

type threadHandlers struct {
	onNotification func(method string, params json.RawMessage)
	onExecApproval func(params execApprovalParams) execApprovalResponse
	onFileApproval func(params fileApprovalParams) fileApprovalResponse
	onElicitation  func(params elicitationParams) elicitationResponse
}

func newCodexClient(rpc *rpcClient) *codexClient {
	c := &codexClient{rpc: rpc, handlers: make(map[string]*threadHandlers)}
	rpc.onNotification = c.dispatchNotification
	rpc.onRequest = c.dispatchRequest
	return c
}

func (c *codexClient) setThreadHandlers(threadID string, h *threadHandlers) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h == nil {
		delete(c.handlers, threadID)
	} else {
		c.handlers[threadID] = h
	}
}

func (c *codexClient) handlersFor(threadID string) *threadHandlers {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handlers[threadID]
}

func (c *codexClient) dispatchNotification(method string, params json.RawMessage) {
	var probe struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &probe)
	if probe.ThreadID == "" {
		return
	}
	if h := c.handlersFor(probe.ThreadID); h != nil && h.onNotification != nil {
		h.onNotification(method, params)
	}
}

// dispatchRequest answers codex's server→client approval requests. With the
// session in a non-bypass mode, codex asks before running exec/file/MCP tools;
// each is forwarded to the ACP client via the registered thread handler.
func (c *codexClient) dispatchRequest(_ context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "item/commandExecution/requestApproval":
		var p execApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		if h := c.handlersFor(p.ThreadID); h != nil && h.onExecApproval != nil {
			return h.onExecApproval(p), nil
		}
		return execApprovalResponse{Decision: "cancel"}, nil
	case "item/fileChange/requestApproval":
		var p fileApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		if h := c.handlersFor(p.ThreadID); h != nil && h.onFileApproval != nil {
			return h.onFileApproval(p), nil
		}
		return fileApprovalResponse{Decision: "cancel"}, nil
	case "mcpServer/elicitation/request":
		var p elicitationParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		if h := c.handlersFor(p.ThreadID); h != nil && h.onElicitation != nil {
			return h.onElicitation(p), nil
		}
		return elicitationResponse{Action: "decline"}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
}

// --- typed wire types (only what we use) ---

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type initializeParams struct {
	ClientInfo   clientInfo `json:"clientInfo"`
	Capabilities any        `json:"capabilities"`
}

type threadStartParams struct {
	Cwd            string         `json:"cwd,omitempty"`
	Model          string         `json:"model,omitempty"`
	ApprovalPolicy any            `json:"approvalPolicy,omitempty"`
	Config         map[string]any `json:"config,omitempty"`
}

type threadStartResponse struct {
	Thread          threadInfo `json:"thread"`
	Model           string     `json:"model"`
	ReasoningEffort *string    `json:"reasoningEffort"`
}

type threadInfo struct {
	ID    string   `json:"id"`
	Cwd   string   `json:"cwd"`
	Turns []rawTurn `json:"turns,omitempty"`
}

// rawTurn keeps item bodies as opaque JSON; LoadSession replays them through
// the same shape the live event dispatcher already understands.
type rawTurn struct {
	ID    string            `json:"id"`
	Items []json.RawMessage `json:"items"`
}

type threadResumeParams struct {
	ThreadID      string         `json:"threadId"`
	Cwd           string         `json:"cwd,omitempty"`
	Model         string         `json:"model,omitempty"`
	ModelProvider string         `json:"modelProvider,omitempty"`
	Config        map[string]any `json:"config,omitempty"`
	// ExcludeTurns skips populating thread.turns. Use for resume without
	// replaying history.
	ExcludeTurns bool `json:"excludeTurns,omitempty"`
}

type threadResumeResponse struct {
	Thread          threadInfo `json:"thread"`
	Model           string     `json:"model"`
	ReasoningEffort *string    `json:"reasoningEffort"`
}

type threadListParams struct {
	Cursor         *string  `json:"cursor,omitempty"`
	Limit          *int     `json:"limit,omitempty"`
	Cwd            any      `json:"cwd,omitempty"` // string | []string
	SourceKinds    []string `json:"sourceKinds,omitempty"`
	ModelProviders []string `json:"modelProviders,omitempty"`
}

type threadListResponse struct {
	Data       []threadSummary `json:"data"`
	NextCursor *string         `json:"nextCursor,omitempty"`
}

type threadSummary struct {
	ID        string  `json:"id"`
	Cwd       string  `json:"cwd"`
	Name      *string `json:"name"`
	Preview   string  `json:"preview"`
	UpdatedAt int64   `json:"updatedAt"`
}

// userInput uses `any` payloads because the variants have disjoint required
// fields; encoding as a tagged struct would force `text_elements` to be
// omitempty (and thus dropped, breaking the codex schema for text inputs).
type turnStartParams struct {
	ThreadID       string `json:"threadId"`
	Input          []any  `json:"input"`
	ApprovalPolicy any    `json:"approvalPolicy,omitempty"`
	SandboxPolicy  any    `json:"sandboxPolicy,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
}

type turnStartResponse struct {
	Turn turn `json:"turn"`
}

type turn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type turnCompleted struct {
	ThreadID string `json:"threadId"`
	Turn     turn   `json:"turn"`
}

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type execApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Reason   string `json:"reason,omitempty"`
	Command  string `json:"command,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
}

type execApprovalResponse struct {
	Decision string `json:"decision"` // accept | acceptForSession | decline | cancel
}

type fileApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Reason   string `json:"reason,omitempty"`
}

type fileApprovalResponse struct {
	Decision string `json:"decision"` // accept | acceptForSession | cancel
}

type elicitationParams struct {
	ThreadID   string `json:"threadId"`
	ServerName string `json:"serverName"`
	Mode       string `json:"mode"` // form | url
	Message    string `json:"message"`
}

// content/_meta are always emitted (null when absent) to match codex's
// McpServerElicitationRequestResponse wire struct.
type elicitationResponse struct {
	Action  string `json:"action"` // accept | decline | cancel
	Content any    `json:"content"`
	Meta    any    `json:"_meta"`
}

// --- typed RPC helpers ---

func (c *codexClient) initialize(ctx context.Context, p initializeParams) error {
	return c.rpc.call(ctx, "initialize", p, nil)
}

func (c *codexClient) threadStart(ctx context.Context, p threadStartParams) (threadStartResponse, error) {
	var out threadStartResponse
	err := c.rpc.call(ctx, "thread/start", p, &out)
	return out, err
}

func (c *codexClient) turnStart(ctx context.Context, p turnStartParams) (turnStartResponse, error) {
	var out turnStartResponse
	err := c.rpc.call(ctx, "turn/start", p, &out)
	return out, err
}

func (c *codexClient) turnInterrupt(ctx context.Context, p turnInterruptParams) error {
	return c.rpc.call(ctx, "turn/interrupt", p, nil)
}

func (c *codexClient) threadResume(ctx context.Context, p threadResumeParams) (threadResumeResponse, error) {
	var out threadResumeResponse
	err := c.rpc.call(ctx, "thread/resume", p, &out)
	return out, err
}

func (c *codexClient) threadList(ctx context.Context, p threadListParams) (threadListResponse, error) {
	var out threadListResponse
	err := c.rpc.call(ctx, "thread/list", p, &out)
	return out, err
}
