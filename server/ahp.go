package server

// AHP is the shared browser and IDE session transport. The native
// controller remains the owner of execution, command receipts and prompts.
// Lock order is sessionController.mu -> ahpHost.mu; never the reverse.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
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
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/coder/websocket"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

type ahpHost struct {
	server       *Server
	mu           sync.Mutex
	createMu     sync.Mutex
	terminalMu   sync.Mutex
	terminalOnce sync.Once
	terminals    map[string]*ahpTerminal
	watches      map[string]*ahpResourceWatch
	watchOnce    sync.Once
	changesMu    sync.Mutex
	changesOnce  sync.Once
	changesets   map[string]*ahpChangeset
	done         chan struct{}
	seq          int64
	clients      map[*ahpConnection]struct{}
	sessions     map[string]*ahpSession
	chats        map[string]*ahpChat
	aliases      map[string]ahpAlias // sessions by URI
	chatAliases  map[string]ahpAlias // forked chats by chat URI
	versions     map[string]ahpClientVersion
	closed       bool
}

// ahpAlias maps an AHP URI to the native session behind it. A session's
// alias names its default chat; a forked chat's alias also names the chat.
type ahpAlias struct {
	Resource           string    `json:"resource"`
	Chat               string    `json:"chat,omitempty"`
	Title              string    `json:"title,omitempty"`
	Backend            string    `json:"backend"`
	Session            string    `json:"session"`
	Fork               string    `json:"fork,omitempty"` // source chat
	Turn               string    `json:"turn,omitempty"` // source turn
	Created            time.Time `json:"created,omitzero"`
	RequestFingerprint string    `json:"requestFingerprint,omitempty"`
	CreationError      string    `json:"creationError,omitempty"`
}

// A client's negotiated version outlives its socket briefly, so a reconnect
// keeps the projection it negotiated.
type ahpClientVersion struct {
	version string
	left    time.Time // zero while a connection with this identity is open
}

const ahpClientVersionTTL = 10 * time.Minute

// ahpHostClient owns terminals that no connected client claims.
const ahpHostClient = "wingman-host"

type ahpConnection struct {
	ctx           context.Context
	host          *ahpHost
	writer        *wsClient
	id            string
	version       string
	initialized   bool
	subscriptions map[string]bool // protected by host.mu
	lanesMu       sync.Mutex
	lanes         map[string]chan ahpRequest
	requests      chan struct{}
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
		h := &ahpHost{server: s, clients: map[*ahpConnection]struct{}{}, sessions: map[string]*ahpSession{}, chats: map[string]*ahpChat{}, aliases: map[string]ahpAlias{}, chatAliases: map[string]ahpAlias{}, versions: map[string]ahpClientVersion{}, terminals: map[string]*ahpTerminal{}, done: make(chan struct{})}
		if dir, err := h.aliasDirectory(); err == nil {
			files, _ := os.ReadDir(dir)
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, file.Name()))
				var alias ahpAlias
				if err == nil && json.Unmarshal(data, &alias) == nil && validAHPSessionURI(alias.Resource) && alias.Backend != "" && alias.Session != "" {
					if alias.Chat != "" {
						h.chatAliases[alias.Chat] = alias
					} else {
						h.aliases[alias.Resource] = alias
					}
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
	return os.Rename(file.Name(), h.aliasPath(dir, alias))
}

func (h *ahpHost) aliasPath(dir string, alias ahpAlias) string {
	key := alias.Resource
	if alias.Chat != "" {
		key = alias.Chat
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// Caller holds h.mu.
func (h *ahpHost) forgetChatAlias(chat string) {
	alias, ok := h.chatAliases[chat]
	if !ok {
		return
	}
	delete(h.chatAliases, chat)
	if dir, err := h.aliasDirectory(); err == nil {
		_ = os.Remove(h.aliasPath(dir, alias))
	}
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

// Web pages always send an http(s) or "null" Origin, so only the same-origin
// Web UI may use one. Native clients send none; desktop web views (VS Code's
// vscode-file://) use their own schemes, which no website can claim.
func ahpOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return strings.EqualFold(u.Host, r.Host)
	}
	return true
}

func (s *Server) handleAHP(w http.ResponseWriter, r *http.Request) {
	if !ahpOriginAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	h := s.ahpHost()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	c := &ahpConnection{host: h, writer: newWSClient(conn), subscriptions: map[string]bool{}, ctx: ctx, lanes: map[string]chan ahpRequest{}, requests: make(chan struct{}, 16)}
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
		cancel()
		h.mu.Lock()
		delete(h.clients, c)
		for _, a := range h.sessions {
			h.removeDetachedClient(a, c.id)
		}
		h.clientLeftLocked(c.id)
		h.releaseTerminalsLocked(c.id)
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
				c.enqueue(req)
			}
			continue
		}
		if !c.initialized && req.Method != "initialize" && req.Method != "reconnect" && req.Method != "ping" {
			if len(req.ID) != 0 {
				c.reply(req.ID, nil, ahpInvalid("initialize first"))
			}
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		// Handshakes and subscriptions enqueue their responses while holding the
		// publication lock, so no live action can overtake its snapshot.
		if req.Method == "initialize" || req.Method == "reconnect" {
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
		// Slow requests (agent startup, session loads, git) must not hold up
		// terminal input or chat actions on the same connection.
		select {
		case c.requests <- struct{}{}:
		default:
			c.reply(req.ID, nil, &ahpRPCError{Code: -32603, Message: "too many concurrent requests"})
			continue
		}
		if req.Method == "subscribe" {
			<-c.requests
			c.enqueue(req)
			continue
		}
		go func() {
			defer func() { <-c.requests }()
			result, err := c.request(req)
			c.reply(req.ID, result, err)
		}()
	}
}

// Subscriptions and actions run in order per channel, and channels run
// independently, so one slow session load never delays another channel.
func (c *ahpConnection) enqueue(req ahpRequest) {
	var p struct {
		Channel string `json:"channel"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.Channel == "" {
		c.reply(req.ID, nil, ahpInvalid("channel required"))
		return
	}
	c.lanesMu.Lock()
	lane := c.lanes[p.Channel]
	if lane == nil {
		if len(c.lanes) >= 256 {
			c.lanesMu.Unlock()
			c.reply(req.ID, nil, ahpInvalid("too many channels"))
			return
		}
		lane = make(chan ahpRequest, 1024)
		c.lanes[p.Channel] = lane
		go func() {
			for {
				select {
				case <-c.ctx.Done():
					return
				case next := <-lane:
					c.channelRequest(next)
				}
			}
		}()
	}
	c.lanesMu.Unlock()
	select {
	case lane <- req:
	default:
		c.writer.close() // a client that floods one channel loses its connection, not its order
	}
}

func (c *ahpConnection) channelRequest(req ahpRequest) {
	switch req.Method {
	case "subscribe":
		if err := c.subscribeRequest(req); err != nil {
			c.reply(req.ID, nil, err)
		}
	case "unsubscribe":
		c.unsubscribe(req.Params)
	default:
		c.dispatch(req.Params)
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
		if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI || p.ClientId == "" || p.ClientId == ahpHostClient {
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
		if json.Unmarshal(req.Params, &p) != nil || p.Channel != t.RootResourceURI || p.ClientId == "" || p.ClientId == ahpHostClient {
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
		if version == "" {
			known, ok := h.versions[clientID]
			if !ok {
				// The host cannot know which projection this client negotiated.
				return ahpInvalid("unknown client; initialize again")
			}
			version = known.version
		}
		h.pruneVersionsLocked()
		h.versions[clientID] = ahpClientVersion{version: version}
		c.id, c.version, c.initialized = clientID, version, true
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
		} else if changeset := h.changesets[channel]; changeset != nil {
			state.Changeset = &changeset.state
		} else {
			a, ch := h.channel(channel)
			if a == nil || a.deleted || ch == nil && channel != a.resource {
				if req.Method == "reconnect" {
					continue
				}
				return &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown channel"}
			}
			if ch == nil {
				state.Session = &a.state
			} else {
				state.Chat = c.chatSnapshot(&ch.chat)
			}
		}
		c.subscriptions[channel] = true
		snapshots = append(snapshots, t.Snapshot{Resource: channel, State: state, FromSeq: h.seq})
	}
	switch req.Method {
	case "initialize":
		dir := h.directoryURI()
		c.reply(req.ID, t.InitializeResult{ProtocolVersion: version, ServerSeq: h.seq, ServerInfo: &t.Implementation{Name: "wingman", Title: ahpPtr("Wingman")}, Snapshots: snapshots, DefaultDirectory: &dir, CompletionTriggerCharacters: []string{"/"}}, nil)
	case "reconnect":
		// AHP explicitly permits snapshot recovery when replay is unavailable.
		c.reply(req.ID, map[string]any{"type": "snapshot", "snapshots": snapshots}, nil)
	case "subscribe":
		c.reply(req.ID, t.SubscribeResult{Snapshot: &snapshots[0]}, nil)
	}
	return nil
}

func (h *ahpHost) clientLeftLocked(id string) {
	for peer := range h.clients {
		if peer.id == id {
			return
		}
	}
	if known, ok := h.versions[id]; ok {
		known.left = time.Now()
		h.versions[id] = known
	}
}

func (h *ahpHost) pruneVersionsLocked() {
	for id, known := range h.versions {
		if !known.left.IsZero() && time.Since(known.left) > ahpClientVersionTTL {
			delete(h.versions, id)
		}
	}
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
	}
	// Running backends are listed even when no registration names them.
	h.server.backendsMu.Lock()
	for id := range h.server.backends {
		if !slices.ContainsFunc(available, func(entry AgentEntry) bool { return entry.ID == id }) {
			available = append(available, AgentEntry{ID: id, Name: id})
		}
	}
	h.server.backendsMu.Unlock()
	// VS Code reads this optional metadata to interpret remote file paths
	// without falling back to its private network diagnostics extension.
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	resources, _ := json.Marshal(map[string]any{"platform": platform, "architecture": runtime.GOARCH, "cpuCount": runtime.NumCPU()})
	root := t.RootState{Agents: []t.AgentInfo{}, Meta: map[string]json.RawMessage{"vscode.agentHost.resources": resources}}
	workspace, _ := json.Marshal(map[string]any{"workspaceId": h.server.scope.WorkspaceID, "instanceId": h.server.scope.InstanceID})
	root.Meta["wingman.workspace"] = workspace
	for _, entry := range available {
		info := t.AgentInfo{Provider: entry.ID, DisplayName: entry.Name, Description: entry.Name + " in Wingman", Models: []t.SessionModelInfo{}, Capabilities: &t.AgentCapabilities{}}
		h.server.backendsMu.Lock()
		backend := h.server.backends[entry.ID]
		var runtime *backendRuntime
		if backend != nil {
			runtime = backend.runtime
		}
		h.server.backendsMu.Unlock()
		if runtime != nil {
			if _, ok := runtime.agent.(code.SessionForker); ok {
				info.Capabilities.MultipleChats = &t.MultipleChatsCapability{Fork: ahpPtr(true)}
			}
		}
		if runtime != nil && runtime.beginOperation() {
			models, _ := runtime.agent.Models("")
			for _, m := range models {
				info.Models = append(info.Models, ahpModelInfo(entry.ID, m))
			}
			runtime.operations.Done()
		}
		root.Agents = append(root.Agents, info)
	}
	return root, nil
}

func ahpModelInfo(provider string, m model.Model) t.SessionModelInfo {
	info := t.SessionModelInfo{Id: m.ID, Name: m.Name, Provider: provider}
	if m.Namespace != "" {
		info.Vendor = &m.Namespace
	}
	if m.Context > 0 {
		info.MaxContextWindow = ahpPtr(int64(m.Context))
		info.MaxPromptTokens = ahpPtr(int64(m.InputTokens()))
	}
	if m.Output > 0 {
		info.MaxOutputTokens = ahpPtr(int64(m.Output))
	}
	if len(m.Efforts) > 0 {
		effort := t.ConfigPropertySchema{Type: "string", Title: "Reasoning effort"}
		for _, value := range m.Efforts {
			data, _ := json.Marshal(value)
			effort.Enum = append(effort.Enum, data)
		}
		if m.Effort != "" {
			data, _ := json.Marshal(m.Effort)
			effort.Default = ahpPtr(json.RawMessage(data))
		}
		info.ConfigSchema = &t.ConfigSchema{Type: "object", Properties: map[string]t.ConfigPropertySchema{"effort": effort}}
	}
	return info
}

// backend resolves an agent provider, reporting an unknown one as AHP's
// ProviderNotFound rather than a startup failure.
func (h *ahpHost) backend(provider string) (*backendRuntime, error) {
	b, err := h.server.backend(provider)
	if err == nil {
		return b, nil
	}
	known := provider == code.BuiltinAgentName
	if h.server.workspace != nil && !known {
		external, _ := agents.Available()
		known = slices.ContainsFunc(external, func(a agents.Registration) bool { return a.ID == provider })
	}
	if !known {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeProviderNotFound), Message: "unknown agent provider: " + provider}
	}
	return nil, err
}

func validAHPSessionURI(resource string) bool {
	return validAHPURI(resource, "ahp-session")
}

func validAHPURI(resource, scheme string) bool {
	u, err := url.Parse(resource)
	return err == nil && u.Scheme == scheme && u.Host == "" && strings.HasPrefix(u.Path, "/") && len(u.Path) > 1 && u.RawQuery == "" && u.Fragment == "" && len(resource) < 2048
}

func canonicalAHPURI(backend, id string) string {
	return "ahp-session:/wingman/" + base64.RawURLEncoding.EncodeToString([]byte(backend)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func ahpChatURI(resource string) string {
	return strings.Replace(resource, "ahp-session:", "ahp-chat:", 1)
}

// channel finds a loaded session or chat. Caller holds h.mu.
func (h *ahpHost) channel(channel string) (*ahpSession, *ahpChat) {
	if ch := h.chats[channel]; ch != nil {
		return ch.session, ch
	}
	return h.sessions[channel], nil
}

// sessionURI names the session a session or chat channel belongs to. Caller holds h.mu.
func (h *ahpHost) sessionURI(channel string) string {
	if alias, ok := h.chatAliases[channel]; ok {
		return alias.Resource
	}
	return strings.Replace(channel, "ahp-chat:", "ahp-session:", 1)
}

// ahpMoved tells a client that a canonical URI's session lives under an alias.
func ahpMoved(resource string) error {
	return &ahpRPCError{Code: -32602, Message: "session is available at " + resource, Data: map[string]string{"resource": resource}}
}

// ensureSession loads the session behind a session or chat channel. The chat
// is nil for the session channel itself.
func (h *ahpHost) ensureSession(channel string) (*ahpSession, *ahpChat, error) {
	h.mu.Lock()
	a, ch := h.channel(channel)
	resource := h.sessionURI(channel)
	h.mu.Unlock()
	if a == nil {
		if _, err := h.loadSession(resource); err != nil {
			return nil, nil, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ch = h.channel(channel)
	if a == nil || a.deleted {
		return nil, nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "session disposed"}
	}
	if ch == nil && channel != a.resource {
		return nil, nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown chat"}
	}
	return a, ch, nil
}

func (h *ahpHost) loadSession(resource string) (*ahpSession, error) {
	if !validAHPSessionURI(resource) {
		return nil, ahpInvalid("unsupported channel")
	}
	h.mu.Lock()
	alias, ok := h.aliases[resource]
	if !ok {
		h.mu.Unlock()
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
		h.mu.Lock()
		for _, other := range h.aliases {
			if other.Backend == alias.Backend && other.Session == alias.Session {
				h.mu.Unlock()
				return nil, ahpMoved(other.Resource)
			}
		}
		for _, other := range h.chatAliases {
			if other.Backend == alias.Backend && other.Session == alias.Session {
				h.mu.Unlock()
				return nil, ahpMoved(other.Resource)
			}
		}
	}
	forks := []ahpAlias{}
	for _, fork := range h.chatAliases {
		if fork.Resource == resource {
			forks = append(forks, fork)
		}
	}
	h.mu.Unlock()
	slices.SortFunc(forks, func(a, b ahpAlias) int { return a.Created.Compare(b.Created) })
	c, title, updated, err := h.loadController(alias.Backend, alias.Session)
	if err != nil {
		return nil, err
	}
	main, err := h.attachSession(c, resource, title, updated)
	if err != nil {
		return nil, err
	}
	a := main.session
	for _, fork := range forks {
		c, _, _, err := h.loadController(fork.Backend, fork.Session)
		var rpc *ahpRPCError
		if errors.As(err, &rpc) && rpc.Code == int(t.ErrorCodeSessionNotFound) {
			h.mu.Lock()
			h.forgetChatAlias(fork.Chat)
			h.mu.Unlock()
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := h.attachChat(a, c, fork); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// loadController loads a native session with its catalog title and time.
func (h *ahpHost) loadController(backend, id string) (*sessionController, string, time.Time, error) {
	b, err := h.backend(backend)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if !b.beginOperation() {
		return nil, "", time.Time{}, errors.New("workspace closed")
	}
	defer b.operations.Done()
	c := b.session(id)
	c.opMu.Lock()
	err = c.loadLocked()
	c.opMu.Unlock()
	if err != nil {
		b.discard(id, c)
		if os.IsNotExist(err) || err.Error() == "session not found" || err.Error() == "session deleted" {
			return nil, "", time.Time{}, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
		}
		return nil, "", time.Time{}, err
	}
	// Attaching must not make an old session look recently modified.
	title, updated := "", time.Now()
	if sessions, err := b.agent.ListSessions(b.ctx); err == nil {
		if i := slices.IndexFunc(sessions, func(info code.SessionInfo) bool { return info.ID == id }); i >= 0 {
			title, updated = sessions[i].Title, sessions[i].UpdatedAt
		}
	}
	return c, title, updated, nil
}

// attachSession projects a native session as an AHP session and returns its default chat.
func (h *ahpHost) attachSession(c *sessionController, resource, title string, updated time.Time) (*ahpChat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ahp != nil {
		if c.ahp.session.resource != resource {
			return nil, ahpMoved(c.ahp.session.resource)
		}
		return c.ahp, nil
	}
	a := newAHPSession(h, resource, c.backend.id, title, updated)
	h.sessions[resource] = a
	return a.addChat(c, ahpChatURI(resource), "", nil, time.Time{}), nil
}

func (h *ahpHost) attachChat(a *ahpSession, c *sessionController, alias ahpAlias) (*ahpChat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ahp != nil {
		if c.ahp.chat.Resource != alias.Chat {
			return nil, ahpInvalid("native session is already a chat elsewhere")
		}
		return c.ahp, nil
	}
	if a.deleted {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "session disposed"}
	}
	var origin *t.ChatOrigin
	if alias.Fork != "" {
		origin = &t.ChatOrigin{Value: &t.ChatForkOrigin{Kind: t.ChatOriginKindFork, Chat: alias.Fork, TurnId: alias.Turn}}
	}
	return a.addChat(c, alias.Chat, alias.Title, origin, alias.Created), nil
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
	case "invokeChangesetOperation":
		return h.invokeChangesetOperation(req.Params)
	case "createTerminal":
		return c.createTerminal(req.Params)
	case "createResourceWatch":
		return h.createResourceWatch(req.Params)
	case "disposeTerminal":
		return c.disposeTerminal(base.Channel)
	case "listSessions":
		if base.Channel != t.RootResourceURI {
			return nil, ahpInvalid("root channel required")
		}
		return h.listSessions(req.Params)
	case "createSession":
		return h.createSession(req.Params, c.id)
	case "createChat":
		return h.createChat(req.Params)
	case "disposeChat":
		a, ch, err := h.ensureSession(base.Channel)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		fork := ch != nil && ch != a.chats[0]
		h.mu.Unlock()
		if !fork {
			return nil, ahpInvalid("only forked chats can be disposed; dispose the session instead")
		}
		return struct{}{}, ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Epoch: ch.controller.epoch, Type: "delete"})
	case "resolveSessionConfig":
		var p t.ResolveSessionConfigParams
		if json.Unmarshal(req.Params, &p) != nil {
			return nil, ahpInvalid("invalid session config")
		}
		provider := code.BuiltinAgentName
		if p.Provider != nil {
			provider = *p.Provider
		}
		b, err := h.backend(provider)
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
		return h.completions(req.Params)
	case "fetchTurns":
		var p t.FetchTurnsParams
		if json.Unmarshal(req.Params, &p) != nil || p.Cursor != nil {
			return nil, ahpInvalid("unknown history cursor")
		}
		_, _, err := h.ensureSession(base.Channel)
		return struct{}{}, err
	case "disposeSession":
		a, ch, err := h.ensureSession(base.Channel)
		if err != nil {
			return nil, err
		}
		if ch != nil {
			return nil, ahpInvalid("session channel required")
		}
		h.mu.Lock()
		chats := slices.Clone(a.chats)
		h.mu.Unlock()
		// Forks go first: deleting the default chat's native session removes the session.
		slices.Reverse(chats)
		for _, ch := range chats {
			if err := ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Epoch: ch.controller.epoch, Type: "delete"}); err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
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
	_, chatTaken := h.chatAliases[ahpChatURI(p.Channel)]
	h.mu.Unlock()
	if chatTaken {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionAlreadyExists), Message: "a chat already uses this session's chat URI"}
	}
	requestData, _ := json.Marshal(p)
	requestHash := sha256.Sum256(requestData)
	fingerprint := hex.EncodeToString(requestHash[:])
	if exists && alias.RequestFingerprint != "" && alias.RequestFingerprint != fingerprint {
		return nil, ahpInvalid("session channel reused with different creation parameters")
	}
	if (exists && alias.CreationError == "") || (!exists && existing != nil) {
		return nil, &ahpRPCError{Code: -32003, Message: "session already exists"}
	}
	b, err := h.backend(provider)
	if err != nil {
		return nil, err
	}
	if !b.beginOperation() {
		return nil, errors.New("workspace closed")
	}
	defer b.operations.Done()
	command := Command{ID: "ahp-create:" + p.Channel, Type: "create"}
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
	main, err := h.attachSession(c, p.Channel, "", time.Now())
	if err != nil {
		return nil, err
	}
	a := main.session
	if len(p.Config) > 0 {
		if err = main.setConfig("ahp-create-config:"+p.Channel, p.Config); err != nil {
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
	return struct{}{}, nil
}

// createChat adds a chat to a session: a fresh native session, or one forked
// from a source chat through one of its turns.
func (h *ahpHost) createChat(raw json.RawMessage) (any, error) {
	var p t.CreateChatParams
	if json.Unmarshal(raw, &p) != nil || !validAHPURI(p.Chat, "ahp-chat") {
		return nil, ahpInvalid("valid chat URI required")
	}
	a, ch, err := h.ensureSession(p.Channel)
	if err != nil {
		return nil, err
	}
	if ch != nil {
		return nil, ahpInvalid("session channel required")
	}
	var fork *t.ForkChatSource
	if p.Source != nil {
		var ok bool
		if fork, ok = p.Source.Value.(*t.ForkChatSource); !ok {
			return nil, ahpInvalid("only forks are supported as chat sources")
		}
	}
	h.createMu.Lock()
	defer h.createMu.Unlock()
	h.mu.Lock()
	_, aliased := h.chatAliases[p.Chat]
	_, sessionAliased := h.aliases[h.sessionURI(p.Chat)]
	if h.chats[p.Chat] != nil || aliased || sessionAliased || h.sessions[h.sessionURI(p.Chat)] != nil || strings.HasPrefix(p.Chat, "ahp-chat:/wingman/") {
		h.mu.Unlock()
		return nil, &ahpRPCError{Code: int(t.ErrorCodeAlreadyExists), Message: "chat already exists"}
	}
	provider := a.state.Provider
	directories := a.state.WorkingDirectories
	command := Command{ID: "ahp-chat:" + p.Chat, Type: "create"}
	if fork != nil {
		source := a.chat(fork.Chat)
		if source == nil {
			h.mu.Unlock()
			return nil, ahpInvalid("fork source must be a chat of this session")
		}
		i := slices.IndexFunc(source.chat.Turns, func(turn t.Turn) bool { return turn.Id == fork.TurnId })
		if i < 0 {
			h.mu.Unlock()
			return nil, ahpInvalid("fork turn must be a completed turn of the source chat")
		}
		command.Type, command.Source = "fork", source.controller.ref.SessionID
		// Forking the whole idle history needs no turn boundary in the native journal.
		if i < len(source.chat.Turns)-1 || source.chat.ActiveTurn != nil {
			command.InputID = fork.TurnId
		}
	}
	h.mu.Unlock()
	if p.Provider != nil && *p.Provider != provider {
		return nil, ahpInvalid("chats use their session's provider")
	}
	for _, dir := range p.WorkingDirectories {
		if fork == nil && !slices.Contains(directories, dir) {
			return nil, ahpInvalid("chat directories must belong to the session")
		}
	}
	if p.Title != nil {
		command.Title = *p.Title
	}
	b, err := h.backend(provider)
	if err != nil {
		return nil, err
	}
	if !b.beginOperation() {
		return nil, errors.New("workspace closed")
	}
	defer b.operations.Done()
	record := b.createSession(command)
	if record.status != 0 {
		return nil, ahpInvalid(record.message)
	}
	c, title, _, err := h.loadController(provider, record.receipt.Ref.SessionID)
	if err != nil {
		return nil, err
	}
	alias := ahpAlias{Resource: a.resource, Chat: p.Chat, Title: cmp.Or(command.Title, title), Backend: provider, Session: record.receipt.Ref.SessionID, Created: time.Now().UTC()}
	if fork != nil {
		alias.Fork, alias.Turn = fork.Chat, fork.TurnId
	}
	if err := h.saveAlias(alias); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.chatAliases[p.Chat] = alias
	h.mu.Unlock()
	if ch, err = h.attachChat(a, c, alias); err != nil {
		return nil, err
	}
	if p.InitialMessage != nil {
		command := Command{ID: ahpCommandID("initial", p.Chat), Epoch: c.epoch, Type: "send", Text: p.InitialMessage.Text, requireIdle: true}
		if err := h.attachments(*p.InitialMessage, &command); err != nil {
			return nil, err
		}
		if err := ch.command(command); err != nil {
			return nil, err
		}
	}
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
			// Forked chats are listed with their session.
			if slices.ContainsFunc(slices.Collect(maps.Values(h.chatAliases)), func(fork ahpAlias) bool { return fork.Backend == b.id && fork.Session == session.ID }) {
				h.mu.Unlock()
				continue
			}
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
			stamp := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
			chat := ahpChatURI(resource)
			chats := []t.SessionChatSummary{{Resource: chat, Title: session.Title}}
			forks := []ahpAlias{}
			for _, fork := range h.chatAliases {
				if fork.Resource == resource {
					forks = append(forks, fork)
				}
			}
			h.mu.Unlock()
			slices.SortFunc(forks, func(a, b ahpAlias) int { return a.Created.Compare(b.Created) })
			for _, fork := range forks {
				chats = append(chats, t.SessionChatSummary{Resource: fork.Chat, Title: fork.Title})
			}
			items = append(items, t.SessionSummary{Resource: resource, Provider: b.id, Title: session.Title, Status: t.SessionStatusIdle, CreatedAt: stamp, ModifiedAt: stamp, WorkingDirectories: []string{h.directoryURI()}, DefaultChat: &chat, Chats: chats})
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

func (c *ahpConnection) unsubscribe(raw json.RawMessage) {
	var p struct {
		Channel string `json:"channel"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	c.host.mu.Lock()
	defer c.host.mu.Unlock()
	delete(c.subscriptions, p.Channel)
	if a, ch := c.host.channel(p.Channel); a != nil && ch == nil {
		c.host.removeDetachedClient(a, c.id)
	}
}

func ahpPtr[T any](value T) *T { return &value }

func (a *ahpChat) command(command Command) error {
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
