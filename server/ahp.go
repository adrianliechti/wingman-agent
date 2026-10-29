package server

// AHP is the shared browser and IDE session transport. The native
// controller remains the owner of execution, command receipts and prompts.
// Lock order is sessionController.mu -> ahpHost.mu; never the reverse.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"github.com/coder/websocket"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type ahpHost struct {
	server             *Server
	mu                 sync.Mutex
	createMu           sync.Mutex
	terminalMu         sync.Mutex
	terminalOnce       sync.Once
	terminals          map[string]*ahpTerminal
	watches            map[string]*ahpResourceWatch
	watchOnce          sync.Once
	changesMu          sync.Mutex
	changesOnce        sync.Once
	changesFingerprint uint64 // protected by changesMu
	changes            *t.ChangesetState
	contents           map[string]string
	done               chan struct{}
	seq                int64
	clients            map[*ahpConnection]struct{}
	sessions           map[string]*ahpSession
	aliases            map[string]ahpAlias
	receipts           map[string]*ahpClientReceipts
	closed             bool
}

type ahpAlias struct {
	Resource           string `json:"resource"`
	Backend            string `json:"backend"`
	Session            string `json:"session"`
	RequestFingerprint string `json:"requestFingerprint,omitempty"`
	CreationError      string `json:"creationError,omitempty"`
}

type ahpClientReceipts struct {
	mu      sync.Mutex // Serialize one identity, including overlapping reconnects.
	version string
	last    int64
	entries map[int64]*ahpActionReceipt
}

type ahpActionReceipt struct {
	fingerprint [32]byte
	envelope    *t.ActionEnvelope
}

type ahpConnection struct {
	resourceCtx   context.Context
	searchMu      sync.Mutex
	searches      map[string]context.CancelFunc
	host          *ahpHost
	writer        *wsClient
	id            string
	version       string
	initialized   bool
	subscriptions map[string]bool // protected by host.mu
}

type ahpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type ahpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *ahpRPCError) Error() string  { return e.Message }
func ahpInvalid(message string) error { return &ahpRPCError{Code: -32602, Message: message} }

func (s *Server) ahpHost() *ahpHost {
	s.ahpOnce.Do(func() {
		h := &ahpHost{server: s, clients: map[*ahpConnection]struct{}{}, sessions: map[string]*ahpSession{}, aliases: map[string]ahpAlias{}, receipts: map[string]*ahpClientReceipts{}, terminals: map[string]*ahpTerminal{}, done: make(chan struct{})}
		if dir, err := h.aliasDirectory(); err == nil {
			files, _ := os.ReadDir(dir)
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, file.Name()))
				var alias ahpAlias
				if err == nil && json.Unmarshal(data, &alias) == nil && validAHPSessionURI(alias.Resource) && alias.Backend != "" && alias.Session != "" {
					h.aliases[alias.Resource] = alias
				}
			}
		}
		s.ahp = h
	})
	return s.ahp
}

func (h *ahpHost) aliasDirectory() (string, error) {
	return layout.WingmanPath("ahp", h.server.scope.WorkspaceID)
}

func (h *ahpHost) saveAlias(alias ahpAlias) error {
	dir, err := h.aliasDirectory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(alias)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(alias.Resource))
	return os.Rename(file.Name(), filepath.Join(dir, hex.EncodeToString(sum[:])+".json"))
}

func (h *ahpHost) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
	for c := range h.clients {
		c.writer.close()
		_ = c.writer.conn.CloseNow()
	}
}

func (s *Server) handleAHP(w http.ResponseWriter, r *http.Request) {
	// The local AHP endpoint intentionally accepts clients from any origin.
	// The server's loopback listener and Host validation still apply.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	h := s.ahpHost()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	c := &ahpConnection{host: h, writer: newWSClient(conn), subscriptions: map[string]bool{}, resourceCtx: ctx, searches: map[string]context.CancelFunc{}}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); c.writer.run() }()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		for _, a := range h.sessions {
			h.removeDetachedClient(a, c.id)
		}
		h.mu.Unlock()
		c.writer.close()
		_ = conn.CloseNow()
		<-done
	}()
	for {
		_, data, err := conn.Read(s.ctx)
		if err != nil {
			return
		}
		var req ahpRequest
		if json.Unmarshal(data, &req) != nil {
			c.reply(json.RawMessage("null"), nil, &ahpRPCError{Code: -32700, Message: "invalid JSON"})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			c.reply(req.ID, nil, &ahpRPCError{Code: -32600, Message: "invalid JSON-RPC request"})
			continue
		}
		if req.Method == "dispatchAction" || req.Method == "unsubscribe" {
			if len(req.ID) != 0 {
				c.reply(req.ID, nil, ahpInvalid("method must be a notification"))
				continue
			}
			if c.initialized {
				c.notification(req)
			}
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		// Handshakes and subscriptions enqueue their responses while holding the
		// publication lock, so no live action can overtake its snapshot.
		if req.Method == "initialize" || req.Method == "reconnect" || req.Method == "subscribe" {
			if err := c.subscribeRequest(req); err != nil {
				c.reply(req.ID, nil, err)
				var rpc *ahpRPCError
				if errors.As(err, &rpc) && rpc.Code == -32005 {
					// Version negotiation failures terminate the connection after
					// the client has received the supported version list.
					c.writer.close()
					<-done
					return
				}
			}
			continue
		}
		if req.Method == "x-wingman/textSearch" {
			c.startFileSearch(req)
			continue
		}
		result, err := c.request(req)
		c.reply(req.ID, result, err)
	}
}

func (c *ahpConnection) reply(id json.RawMessage, result any, err error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if err != nil {
		var rpc *ahpRPCError
		if !errors.As(err, &rpc) {
			rpc = &ahpRPCError{Code: -32603, Message: err.Error()}
			if os.IsNotExist(err) {
				rpc.Code = int(t.ErrorCodeNotFound)
			} else if os.IsPermission(err) {
				rpc.Code = int(t.ErrorCodePermissionDenied)
			}
		}
		c.writer.deliver(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *ahpRPCError    `json:"error"`
		}{"2.0", id, rpc})
		return
	}
	if result == nil {
		result = struct{}{}
	}
	c.writer.deliver(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result})
}

func (c *ahpConnection) notify(method string, params any) {
	c.writer.deliver(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", method, params})
}

func (h *ahpHost) notifyRoot(method string, params any) {
	for c := range h.clients {
		if c.subscriptions[t.RootResourceURI] {
			c.notify(method, params)
		}
	}
}

func tSessionAdded(a *ahpSession) t.SessionAddedParams {
	return t.SessionAddedParams{Channel: t.RootResourceURI, Summary: a.summary()}
}

// A reconnect may overlap the old socket closing. Keep the client's session
// presence while another connection with that identity is subscribed.
func (h *ahpHost) removeDetachedClient(a *ahpSession, id string) {
	for c := range h.clients {
		if c.id == id && c.subscriptions[a.resource] {
			return
		}
	}
	a.removeClient(id)
}

func (h *ahpHost) publishAgents() {
	root, err := h.root()
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.publish(t.RootResourceURI, t.StateAction{Value: &t.RootAgentsChangedAction{Type: t.ActionTypeRootAgentsChanged, Agents: root.Agents}}, nil, nil)
}

func (h *ahpHost) publish(channel string, action t.StateAction, origin *t.ActionOrigin, rejection *string) t.ActionEnvelope {
	// 1.0 reducers keep queued and steering input in separate slots. Clear the
	// opposite slot before a kind change, without changing 1.1's stable ordering.
	if set, ok := action.Value.(*t.ChatPendingMessageSetAction); ok && rejection == nil {
		h.seq++
		kind := t.PendingMessageKindQueued
		if set.Kind == kind {
			kind = t.PendingMessageKindSteering
		}
		remove := t.StateAction{Value: &t.ChatPendingMessageRemovedAction{Type: t.ActionTypeChatPendingMessageRemoved, Kind: kind, Id: set.Id}}
		for c := range h.clients {
			if c.legacyQueue() && c.subscriptions[channel] {
				c.notify("action", t.ActionEnvelope{Channel: channel, Action: remove, ServerSeq: h.seq})
			}
		}
	}
	h.seq++
	envelope := t.ActionEnvelope{Channel: channel, Action: action, ServerSeq: h.seq, Origin: origin, RejectionReason: rejection}
	for c := range h.clients {
		if c.subscriptions[channel] || origin != nil && origin.ClientId == c.id {
			c.notify("action", envelope)
		}
	}
	return envelope
}

func (c *ahpConnection) legacyQueue() bool {
	return c.version == "" || strings.HasPrefix(c.version, "0.") || strings.HasPrefix(c.version, "1.0.")
}

func (c *ahpConnection) chatSnapshot(chat *t.ChatState) *t.ChatState {
	if !c.legacyQueue() {
		return chat
	}
	state := *chat
	state.QueuedMessages = nil
	for _, message := range chat.QueuedMessages {
		if message.Kind == nil || *message.Kind == t.PendingMessageKindQueued {
			state.QueuedMessages = append(state.QueuedMessages, message)
		}
	}
	return &state
}

func (c *ahpConnection) subscribeRequest(req ahpRequest) error {
	h := c.host
	var channels []string
	var clientID, version string
	switch req.Method {
	case "initialize":
		if c.initialized {
			return ahpInvalid("already initialized")
		}
		var p t.InitializeParams
		if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI || p.ClientId == "" {
			return ahpInvalid("root channel and clientId required")
		}
		var err error
		version, err = t.NegotiateProtocolVersion(p.ProtocolVersions)
		if err != nil {
			return ahpInvalid(err.Error())
		}
		if version == "" {
			return &ahpRPCError{Code: -32005, Message: "unsupported protocol version", Data: map[string]any{"supportedVersions": t.SupportedProtocolVersions()}}
		}
		clientID, channels = p.ClientId, p.InitialSubscriptions
	case "reconnect":
		if c.initialized {
			return ahpInvalid("already initialized")
		}
		var p t.ReconnectParams
		if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI || p.ClientId == "" {
			return ahpInvalid("root channel and clientId required")
		}
		clientID, channels = p.ClientId, p.Subscriptions
	case "subscribe":
		if !c.initialized {
			return ahpInvalid("initialize first")
		}
		var p t.SubscribeParams
		if json.Unmarshal(req.Params, &p) != nil || p.Channel == "" {
			return ahpInvalid("channel required")
		}
		channels = []string{p.Channel}
	}
	if len(channels) > 128 {
		return ahpInvalid("too many subscriptions")
	}
	var root *t.RootState
	for _, channel := range channels {
		if channel == t.RootResourceURI {
			state, err := h.root()
			if err != nil {
				return err
			}
			root = &state
		} else if err := h.prepareChannel(channel); err != nil {
			if req.Method != "reconnect" {
				return err
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if clientID != "" {
		if len(h.receipts) >= 1024 && h.receipts[clientID] == nil {
			return errors.New("too many AHP client identities; restart the server")
		}
		if h.receipts[clientID] == nil {
			h.receipts[clientID] = &ahpClientReceipts{entries: map[int64]*ahpActionReceipt{}}
		}
		c.id = clientID
		if version != "" {
			h.receipts[clientID].version = version
		}
		c.version = h.receipts[clientID].version
		c.initialized = true
	}
	if len(c.subscriptions)+len(channels) > 128 {
		return ahpInvalid("too many subscriptions")
	}
	snapshots := []t.Snapshot{}
	for _, channel := range channels {
		var state t.SnapshotState
		if channel == t.RootResourceURI {
			root.Terminals = h.terminalCatalogueLocked()
			state.Root = root
		} else if terminal := h.terminals[channel]; terminal != nil {
			state.Terminal = &terminal.state
		} else if watch := h.watches[channel]; watch != nil {
			state.ResourceWatch = &watch.state
			watch.touched = time.Now()
		} else if channel == ahpWorkspaceChanges && h.changes != nil {
			state.Changeset = h.changes
		} else {
			a := h.sessionByChannel(channel)
			if a == nil || a.deleted {
				if req.Method == "reconnect" {
					continue
				}
				return &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown channel"}
			}
			if channel == a.resource {
				state.Session = &a.state
			} else {
				state.Chat = c.chatSnapshot(&a.chat)
			}
		}
		c.subscriptions[channel] = true
		snapshots = append(snapshots, t.Snapshot{Resource: channel, State: state, FromSeq: h.seq})
	}
	switch req.Method {
	case "initialize":
		dir := h.directoryURI()
		c.reply(req.ID, t.InitializeResult{ProtocolVersion: version, ServerSeq: h.seq, ServerInfo: &t.Implementation{Name: "wingman", Title: ahpPtr("Wingman")}, Snapshots: snapshots, DefaultDirectory: &dir}, nil)
	case "reconnect":
		// AHP explicitly permits snapshot recovery when replay is unavailable.
		c.reply(req.ID, map[string]any{"type": "snapshot", "snapshots": snapshots}, nil)
	case "subscribe":
		c.reply(req.ID, t.SubscribeResult{Snapshot: &snapshots[0]}, nil)
	}
	return nil
}

func (h *ahpHost) directoryURI() string {
	root := "/"
	if h.server.workspace != nil {
		root = h.server.workspace.RootPath
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(root)}).String()
}

func (h *ahpHost) root() (t.RootState, error) {
	h.startTerminals()
	available := []AgentEntry{{ID: code.BuiltinAgentName, Name: "Wingman"}}
	if h.server.workspace != nil {
		external, err := agents.Available()
		if err != nil {
			return t.RootState{}, err
		}
		for _, a := range external {
			available = append(available, AgentEntry{ID: a.ID, Name: a.Name})
		}
	} else {
		available = nil
		h.server.backendsMu.Lock()
		for id := range h.server.backends {
			available = append(available, AgentEntry{ID: id, Name: id})
		}
		h.server.backendsMu.Unlock()
	}
	// VS Code reads this optional metadata to interpret remote file paths
	// without falling back to its private network diagnostics extension.
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	resources, _ := json.Marshal(map[string]any{"platform": platform, "architecture": runtime.GOARCH, "cpuCount": runtime.NumCPU()})
	root := t.RootState{Agents: []t.AgentInfo{}, Meta: map[string]json.RawMessage{"vscode.agentHost.resources": resources}}
	workspace, _ := json.Marshal(map[string]any{"workspaceId": h.server.scope.WorkspaceID, "instanceId": h.server.scope.InstanceID, "directory": h.directoryURI()})
	root.Meta["wingman"] = workspace
	for _, entry := range available {
		info := t.AgentInfo{Provider: entry.ID, DisplayName: entry.Name, Description: entry.Name + " in Wingman", Models: []t.SessionModelInfo{}}
		h.server.backendsMu.Lock()
		backend := h.server.backends[entry.ID]
		var runtime *backendRuntime
		if backend != nil {
			runtime = backend.runtime
		}
		h.server.backendsMu.Unlock()
		if runtime != nil && runtime.beginOperation() {
			models, _ := runtime.agent.Models("")
			for _, m := range models {
				info.Models = append(info.Models, t.SessionModelInfo{Id: m.ID, Name: m.Name, Provider: entry.ID})
			}
			runtime.operations.Done()
		}
		root.Agents = append(root.Agents, info)
	}
	return root, nil
}

func validAHPSessionURI(resource string) bool {
	u, err := url.Parse(resource)
	return err == nil && u.Scheme == "ahp-session" && u.Host == "" && strings.HasPrefix(u.Path, "/") && len(u.Path) > 1 && u.RawQuery == "" && u.Fragment == "" && len(resource) < 2048
}

func canonicalAHPURI(backend, id string) string {
	return "ahp-session:/wingman/" + base64.RawURLEncoding.EncodeToString([]byte(backend)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func ahpChatURI(resource string) string {
	return strings.Replace(resource, "ahp-session:", "ahp-chat:", 1)
}

func (h *ahpHost) sessionByChannel(channel string) *ahpSession {
	if strings.HasPrefix(channel, "ahp-chat:") {
		channel = strings.Replace(channel, "ahp-chat:", "ahp-session:", 1)
	}
	return h.sessions[channel]
}

func (h *ahpHost) ensureSession(channel string) (*ahpSession, error) {
	resource := strings.Replace(channel, "ahp-chat:", "ahp-session:", 1)
	if !validAHPSessionURI(resource) {
		return nil, ahpInvalid("unsupported channel")
	}
	h.mu.Lock()
	a := h.sessions[resource]
	alias, ok := h.aliases[resource]
	if a != nil {
		if a.deleted {
			h.mu.Unlock()
			return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "session disposed"}
		}
		h.mu.Unlock()
		return a, nil
	}
	h.mu.Unlock()
	if !ok {
		parts := strings.Split(strings.TrimPrefix(resource, "ahp-session:/wingman/"), "/")
		if !strings.HasPrefix(resource, "ahp-session:/wingman/") || len(parts) != 2 {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
		}
		backend, e1 := base64.RawURLEncoding.DecodeString(parts[0])
		id, e2 := base64.RawURLEncoding.DecodeString(parts[1])
		if e1 != nil || e2 != nil || len(id) == 0 {
			return nil, ahpInvalid("invalid session URI")
		}
		alias = ahpAlias{Resource: resource, Backend: string(backend), Session: string(id)}
	}
	b, err := h.server.backend(alias.Backend)
	if err != nil {
		return nil, err
	}
	if !b.beginOperation() {
		return nil, errors.New("workspace closed")
	}
	defer b.operations.Done()
	c := b.session(alias.Session)
	c.opMu.Lock()
	err = c.loadLocked()
	c.opMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) || err.Error() == "session not found" || err.Error() == "session deleted" {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
		}
		return nil, err
	}
	a = h.attach(c, resource, "", time.Now())
	if a.resource != resource {
		return nil, ahpInvalid("session is available at " + a.resource)
	}
	return a, nil
}

func (h *ahpHost) attach(c *sessionController, resource, title string, updated time.Time) *ahpSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ahp != nil {
		return c.ahp
	}
	if title == "" {
		title = "New session"
	}
	a := newAHPSession(h, c, resource, title, updated)
	c.ahp = a
	h.sessions[resource] = a
	a.syncProjection(c, nil)
	return a
}

func (c *ahpConnection) request(req ahpRequest) (any, error) {
	if req.Method == "ping" {
		return struct{}{}, nil
	}
	if !c.initialized {
		return nil, ahpInvalid("initialize first")
	}
	h := c.host
	var base struct {
		Channel string `json:"channel"`
	}
	if json.Unmarshal(req.Params, &base) != nil || base.Channel == "" {
		return nil, ahpInvalid("channel required")
	}
	switch req.Method {
	case "x-wingman/fileSearch", "x-wingman/revealResource":
		return h.fileExtension(req)
	case "x-wingman/cancelSearch":
		return c.cancelFileSearch(req.Params)
	case "createTerminal":
		return c.createTerminal(req.Params)
	case "createResourceWatch":
		return h.createResourceWatch(req.Params)
	case "x-wingman/resolveSession":
		var p struct {
			Backend string `json:"backend"`
			Session string `json:"session"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.Backend == "" || p.Session == "" {
			return nil, ahpInvalid("session identity required")
		}
		resource := canonicalAHPURI(p.Backend, p.Session)
		h.mu.Lock()
		for uri, alias := range h.aliases {
			if alias.Backend == p.Backend && alias.Session == p.Session {
				resource = uri
				break
			}
		}
		h.mu.Unlock()
		return map[string]string{"resource": resource}, nil
	case "x-wingman/compact", "x-wingman/queueControl":
		return h.sessionExtension(req)
	case "x-wingman/terminalStatus":
		h.mu.Lock()
		terminal := h.terminals[base.Channel]
		h.mu.Unlock()
		if terminal == nil {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "terminal not found"}
		}
		return map[string]bool{"busy": terminal.session.HasForegroundProcess()}, nil
	case "x-wingman/focus":
		h.server.flushFiles()
		return struct{}{}, nil
	case "disposeTerminal":
		return c.disposeTerminal(base.Channel)
	case "listSessions":
		if base.Channel != t.RootResourceURI {
			return nil, ahpInvalid("root channel required")
		}
		return h.listSessions(req.Params)
	case "createSession":
		return h.createSession(req.Params, c.id)
	case "resolveSessionConfig":
		var p t.ResolveSessionConfigParams
		if json.Unmarshal(req.Params, &p) != nil {
			return nil, ahpInvalid("invalid session config")
		}
		provider := code.BuiltinAgentName
		if p.Provider != nil {
			provider = *p.Provider
		}
		b, err := h.server.backend(provider)
		if err != nil {
			return nil, err
		}
		if !b.beginOperation() {
			return nil, errors.New("workspace closed")
		}
		defer b.operations.Done()
		cfg := ahpConfig(b.settings(""))
		h.publishAgents()
		for key, value := range p.Config {
			if _, ok := cfg.Schema.Properties[key]; ok {
				cfg.Values[key] = value
			}
		}
		return t.ResolveSessionConfigResult{Schema: cfg.Schema, Values: cfg.Values}, nil
	case "sessionConfigCompletions":
		return t.SessionConfigCompletionsResult{Items: []t.SessionConfigValueItem{}}, nil
	case "completions":
		return t.CompletionsResult{Items: []t.CompletionItem{}}, nil
	case "fetchTurns":
		var p t.FetchTurnsParams
		if json.Unmarshal(req.Params, &p) != nil || p.Cursor != nil {
			return nil, ahpInvalid("unknown history cursor")
		}
		_, err := h.ensureSession(base.Channel)
		return struct{}{}, err
	case "disposeSession":
		a, err := h.ensureSession(base.Channel)
		if err != nil {
			return nil, err
		}
		return struct{}{}, a.command(Command{ID: "ahp-dispose:" + a.resource, Epoch: a.controller.epoch, Type: "delete"})
	case "resourceList", "resourceRead", "resourceResolve", "resourceRequest", "resourceWrite", "resourceCopy", "resourceMove", "resourceDelete", "resourceMkdir":
		result, err := h.resource(req)
		return result, ahpResourceError(err)
	default:
		return nil, &ahpRPCError{Code: -32601, Message: "method not supported: " + req.Method}
	}
}

func (h *ahpHost) createSession(raw json.RawMessage, clientID string) (any, error) {
	var p t.CreateSessionParams
	if json.Unmarshal(raw, &p) != nil || !validAHPSessionURI(p.Channel) {
		return nil, ahpInvalid("valid session channel required")
	}
	if p.ActiveClient != nil && p.ActiveClient.ClientId != clientID {
		return nil, ahpInvalid("active client identity mismatch")
	}
	for _, dir := range p.WorkingDirectories {
		if err := h.validateDirectory(dir); err != nil {
			return nil, err
		}
	}
	provider := code.BuiltinAgentName
	if p.Provider != nil {
		provider = *p.Provider
	}
	h.createMu.Lock()
	defer h.createMu.Unlock()
	h.mu.Lock()
	alias, exists := h.aliases[p.Channel]
	existing := h.sessions[p.Channel]
	h.mu.Unlock()
	requestData, _ := json.Marshal(p)
	requestHash := sha256.Sum256(requestData)
	fingerprint := hex.EncodeToString(requestHash[:])
	if exists && alias.RequestFingerprint != "" && alias.RequestFingerprint != fingerprint {
		return nil, ahpInvalid("session channel reused with different creation parameters")
	}
	if (exists && alias.CreationError == "") || (!exists && existing != nil) {
		return nil, &ahpRPCError{Code: -32003, Message: "session already exists"}
	}
	b, err := h.server.backend(provider)
	if err != nil {
		return nil, err
	}
	if !b.beginOperation() {
		return nil, errors.New("workspace closed")
	}
	defer b.operations.Done()
	command := Command{ID: "ahp-create:" + p.Channel, Type: "create"}
	if data := p.Meta["wingman.fork"]; data != nil {
		var fork struct {
			Source string `json:"source"`
			Title  string `json:"title"`
		}
		if json.Unmarshal(data, &fork) != nil {
			return nil, ahpInvalid("invalid fork request")
		}
		source, loadErr := h.ensureSession(fork.Source)
		if loadErr != nil {
			return nil, loadErr
		}
		if _, ok := b.agent.(code.SessionForker); !ok || source.controller.ref.BackendID != provider {
			return nil, ahpInvalid("provider does not support this fork")
		}
		command.Type, command.Source, command.Title = "fork", source.controller.ref.SessionID, fork.Title
	}
	if !exists {
		record := b.createSession(command)
		if record.status != 0 {
			return nil, ahpInvalid(record.message)
		}
		alias = ahpAlias{Resource: p.Channel, Backend: provider, Session: record.receipt.Ref.SessionID, RequestFingerprint: fingerprint, CreationError: "session configuration is incomplete"}
		if err = h.saveAlias(alias); err != nil {
			return nil, err
		}
	}
	id := alias.Session
	h.mu.Lock()
	h.aliases[p.Channel] = alias
	h.mu.Unlock()
	c := b.session(id)
	a := h.attach(c, p.Channel, command.Title, time.Now())
	if len(p.Config) > 0 {
		if err = a.setConfig("ahp-create-config:"+p.Channel, p.Config); err != nil {
			return nil, err
		}
	}
	alias.CreationError = ""
	if err = h.saveAlias(alias); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.aliases[p.Channel] = alias
	h.mu.Unlock()
	h.mu.Lock()
	if p.ActiveClient != nil {
		a.sessionAction(t.StateAction{Value: &t.SessionActiveClientSetAction{Type: t.ActionTypeSessionActiveClientSet, ActiveClient: *p.ActiveClient}})
	}
	h.notifyRoot("root/sessionAdded", t.SessionAddedParams{Channel: t.RootResourceURI, Summary: a.summary()})
	h.mu.Unlock()
	b.ensureTaskPump(id)
	b.broadcast(Frame{Type: EvtSessionsChanged})
	return struct{}{}, nil
}

func (h *ahpHost) listSessions(raw json.RawMessage) (any, error) {
	var p t.ListSessionsParams
	if json.Unmarshal(raw, &p) != nil || p.Cursor != nil {
		return nil, ahpInvalid("invalid session list cursor")
	}
	root, err := h.root()
	if err != nil {
		return nil, err
	}
	items := []t.SessionSummary{}
	seen := map[string]bool{}
	for _, info := range root.Agents {
		// Listing must not launch every installed external CLI. An external
		// backend becomes available here after first use/config discovery.
		h.server.backendsMu.Lock()
		entry := h.server.backends[info.Provider]
		var b *backendRuntime
		if entry != nil {
			b = entry.runtime
		}
		h.server.backendsMu.Unlock()
		if b == nil || !b.beginOperation() {
			continue
		}
		sessions, err := b.agent.ListSessions(b.ctx)
		b.operations.Done()
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			resource := canonicalAHPURI(b.id, session.ID)
			h.mu.Lock()
			for uri, alias := range h.aliases {
				if alias.Backend == b.id && alias.Session == session.ID {
					resource = uri
					break
				}
			}
			if a := h.sessions[resource]; a != nil {
				if !a.deleted {
					items = append(items, a.summary())
					seen[a.resource] = true
				}
				h.mu.Unlock()
				continue
			}
			h.mu.Unlock()
			stamp := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
			chat := ahpChatURI(resource)
			items = append(items, t.SessionSummary{Meta: ahpSummaryMeta(SessionRef{WorkspaceID: b.scope.WorkspaceID, BackendID: b.id, SessionID: session.ID}), Resource: resource, Provider: b.id, Title: session.Title, Status: t.SessionStatusIdle, CreatedAt: stamp, ModifiedAt: stamp, WorkingDirectories: []string{h.directoryURI()}, DefaultChat: &chat})
			seen[resource] = true
		}
	}
	h.mu.Lock()
	for _, a := range h.sessions {
		if !a.deleted && !seen[a.resource] {
			items = append(items, a.summary())
			seen[a.resource] = true
		}
	}
	h.mu.Unlock()
	slices.SortFunc(items, func(a, b t.SessionSummary) int { return strings.Compare(b.ModifiedAt, a.ModifiedAt) })
	// Full retained catalog is returned without a cursor; pagination is optional.
	return t.ListSessionsResult{Items: items}, nil
}

func (c *ahpConnection) notification(req ahpRequest) {
	if req.Method == "unsubscribe" {
		var p struct {
			Channel string `json:"channel"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			return
		}
		c.host.mu.Lock()
		delete(c.subscriptions, p.Channel)
		if a := c.host.sessionByChannel(p.Channel); a != nil && p.Channel == a.resource {
			c.host.removeDetachedClient(a, c.id)
		}
		c.host.mu.Unlock()
		return
	}
	c.dispatch(req.Params)
}

func ahpPtr[T any](value T) *T { return &value }

func (a *ahpSession) command(command Command) error {
	b := a.controller.backend
	if !b.beginOperation() {
		return errors.New("workspace closed")
	}
	defer b.operations.Done()
	r := a.controller.command(b.ctx, command)
	if r.status != 0 {
		return errors.New(r.message)
	}
	return nil
}
