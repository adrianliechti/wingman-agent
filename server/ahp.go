package server

// AHP is the shared browser and IDE session transport. The native
// controller remains the owner of execution, command receipts and prompts.
// Lock order is sessionController.mu -> ahpHost.mu; never the reverse.
// Project startup and teardown hold ahpHost.projectMu, taking mu only to
// change the registry. Backend I/O and resource cleanup happen outside mu.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/coder/websocket"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpHost is the endpoint: its connections, the order of their actions and the channel namespace.
type ahpHost struct {
	mu          sync.Mutex
	creating    map[string]chan struct{} // sessions being created, closed when done
	seq         int64
	clients     map[*ahpConnection]struct{}
	sessions    map[string]*ahpSession
	chats       map[string]*ahpChat
	aliases     map[string]ahpAlias // sessions by URI
	chatAliases map[string]ahpAlias // forked chats by chat URI
	versions    map[string]ahpClientVersion
	closed      bool
	pinned      *ahpProject // the one project of a workspace server; otherwise projects open on demand
	projects    map[string]*ahpProject
	open        func(dir string) (*Server, error)
	projectMu   sync.Mutex // serializes project startup and teardown, outside mu
	terminalMu  sync.Mutex // orders terminal creation: a name is claimed in every project at once
	roots       []string   // directories projects may be opened in
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

func newAHPHost() *ahpHost {
	return &ahpHost{creating: map[string]chan struct{}{}, clients: map[*ahpConnection]struct{}{}, sessions: map[string]*ahpSession{}, chats: map[string]*ahpChat{}, aliases: map[string]ahpAlias{}, chatAliases: map[string]ahpAlias{}, versions: map[string]ahpClientVersion{}, projects: map[string]*ahpProject{}}
}

func (h *ahpHost) connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

func (h *ahpHost) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, p := range h.projectsLocked() {
		p.stop()
	}
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
	s.ahp.serve(s.ctx, w, r)
}

// serve runs one client connection until it closes or parent ends.
func (h *ahpHost) serve(parent context.Context, w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithCancel(parent)
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
		for _, p := range h.projectsLocked() {
			p.releaseTerminalsLocked(c.id)
		}
		h.mu.Unlock()
		c.writer.close()
		_ = conn.CloseNow()
		<-done
	}()
	for {
		_, data, err := conn.Read(parent)
		if err != nil {
			return
		}
		var req ahpRequest
		if json.Unmarshal(data, &req) != nil {
			// Valid JSON that is no request object is an invalid request, not a parse error.
			if json.Valid(data) {
				c.reply(json.RawMessage("null"), nil, &ahpRPCError{Code: -32600, Message: "invalid JSON-RPC request"})
			} else {
				c.reply(json.RawMessage("null"), nil, &ahpRPCError{Code: -32700, Message: "invalid JSON"})
			}
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
				// VS Code handles NotFound by initializing again on this socket;
				// InvalidParams leaves it retrying reconnect indefinitely.
				return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "unknown client; initialize again"}
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
			root.Terminals = h.terminalsLocked()
			state.Root = root
		} else if p := h.ownerLocked(channel); p != nil && p.snapshotLocked(channel, &state) {
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
		dir := h.defaultDirectory()
		pathStyle := t.PathStylePosix
		if runtime.GOOS == "windows" {
			pathStyle = t.PathStyleWindows
		}
		c.reply(req.ID, t.InitializeResult{ProtocolVersion: version, ServerSeq: h.seq, ServerInfo: &t.Implementation{Name: "wingman", Title: new("Wingman")}, Snapshots: snapshots, DefaultDirectory: &dir, Environment: &t.HostEnvironment{PathStyle: &pathStyle}, CompletionTriggerCharacters: []string{"/"}}, nil)
	case "reconnect":
		// AHP explicitly permits snapshot recovery when replay is unavailable.
		c.reply(req.ID, t.ReconnectResult{Value: &t.ReconnectSnapshotResult{Snapshots: snapshots}}, nil)
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

// Models are known only for agents that a project has started.
func (h *ahpHost) root() (t.RootState, error) {
	projects := h.loaded()
	for _, p := range projects {
		p.startTerminals()
	}
	available := []AgentEntry{{ID: code.BuiltinAgentName, Name: "Wingman"}}
	if h.pinned == nil || h.pinned.server.workspace != nil {
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
	for _, p := range projects {
		p.server.backendsMu.Lock()
		for id := range p.server.backends {
			if !slices.ContainsFunc(available, func(entry AgentEntry) bool { return entry.ID == id }) {
				available = append(available, AgentEntry{ID: id, Name: id})
			}
		}
		p.server.backendsMu.Unlock()
	}
	// VS Code reads this optional metadata to interpret remote file paths
	// without falling back to its private network diagnostics extension.
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	resources, _ := json.Marshal(map[string]any{"platform": platform, "architecture": runtime.GOARCH, "cpuCount": runtime.NumCPU()})
	root := t.RootState{Agents: []t.AgentInfo{}, Meta: map[string]json.RawMessage{"vscode.agentHost.resources": resources}}
	if h.pinned != nil {
		scope := h.pinned.server.scope
		workspace, _ := json.Marshal(map[string]any{"workspaceId": scope.WorkspaceID, "instanceId": scope.InstanceID})
		root.Meta["wingman.workspace"] = workspace
	}
	for _, entry := range available {
		info := t.AgentInfo{Provider: entry.ID, DisplayName: entry.Name, Description: entry.Name + " in Wingman", Models: []t.SessionModelInfo{}, Capabilities: &t.AgentCapabilities{}}
		runtime := startedBackend(projects, entry.ID)
		if runtime != nil {
			if _, ok := runtime.agent.(code.SessionForker); ok {
				info.Capabilities.MultipleChats = &t.MultipleChatsCapability{Fork: new(true)}
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

func startedBackend(projects []*ahpProject, provider string) *backendRuntime {
	for _, p := range projects {
		if runtime := p.startedBackend(provider); runtime != nil {
			return runtime
		}
	}
	return nil
}

// defaultDirectory is where a client starts choosing a working directory.
func (h *ahpHost) defaultDirectory() string {
	if h.pinned != nil {
		return h.pinned.directoryURI()
	}
	return fileuri.FromPath(h.roots[0])
}

func ahpModelInfo(provider string, m model.Model) t.SessionModelInfo {
	info := t.SessionModelInfo{Id: m.ID, Name: m.Name, Provider: provider}
	if m.Namespace != "" {
		info.Vendor = &m.Namespace
	}
	if m.Context > 0 {
		info.MaxContextWindow = new(int64(m.Context))
		info.MaxPromptTokens = new(int64(m.InputTokens()))
	}
	if m.Output > 0 {
		info.MaxOutputTokens = new(int64(m.Output))
	}
	if len(m.Efforts) > 0 {
		effort := t.ConfigPropertySchema{Type: "string", Title: "Reasoning effort"}
		for _, value := range m.Efforts {
			data, _ := json.Marshal(value)
			effort.Enum = append(effort.Enum, data)
		}
		if m.Effort != "" {
			data, _ := json.Marshal(m.Effort)
			effort.Default = new(json.RawMessage(data))
		}
		info.ConfigSchema = &t.ConfigSchema{Type: "object", Properties: map[string]t.ConfigPropertySchema{"effort": effort}}
	}
	return info
}

func (c *ahpConnection) request(req ahpRequest) (any, error) {
	if req.Method == "ping" {
		return nil, nil
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
		p := h.owner(base.Channel)
		if p == nil {
			return nil, ahpInvalid("changeset has no operations")
		}
		return p.invokeChangesetOperation(req.Params)
	case "createTerminal":
		return c.createTerminal(req.Params)
	case "createResourceWatch":
		var watch t.CreateResourceWatchParams
		if json.Unmarshal(req.Params, &watch) != nil || watch.Channel != t.RootResourceURI {
			return nil, ahpInvalid("root channel required")
		}
		p, err := h.loadedProject(&watch.Uri)
		if err != nil {
			return nil, err
		}
		return p.createResourceWatch(req.Params)
	case "disposeTerminal":
		return c.disposeTerminal(base.Channel)
	case "listSessions":
		if base.Channel != t.RootResourceURI {
			return nil, ahpInvalid("root channel required")
		}
		return h.listSessions(req.Params)
	case "createSession":
		var session t.CreateSessionParams
		if json.Unmarshal(req.Params, &session) != nil {
			return nil, ahpInvalid("valid session channel required")
		}
		var directory *string
		if len(session.WorkingDirectories) > 0 {
			directory = &session.WorkingDirectories[0]
		}
		p, err := h.workingProject(directory)
		if err != nil {
			return nil, err
		}
		return p.createSession(req.Params, c.id)
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
		return nil, ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Type: "delete"})
	case "resolveSessionConfig":
		var p t.ResolveSessionConfigParams
		if json.Unmarshal(req.Params, &p) != nil {
			return nil, ahpInvalid("invalid session config")
		}
		provider := code.BuiltinAgentName
		if p.Provider != nil {
			provider = *p.Provider
		}
		project, err := h.workingProject(p.WorkingDirectory)
		if err != nil {
			return nil, err
		}
		started := project.startedBackend(provider) != nil
		b, err := project.backend(provider)
		if err != nil {
			return nil, err
		}
		if !b.beginOperation() {
			return nil, errors.New("workspace closed")
		}
		defer b.operations.Done()
		cfg := ahpConfig(b.settings(""))
		// An agent that just started may know its models.
		if !started {
			h.publishAgents()
		}
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
			if err := ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Type: "delete"}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "resourceList", "resourceRead", "resourceResolve", "resourceRequest", "resourceWrite", "resourceCopy", "resourceMove", "resourceDelete", "resourceMkdir":
		result, err := h.resource(req)
		return result, ahpResourceError(err)
	default:
		return nil, &ahpRPCError{Code: -32601, Message: "method not supported: " + req.Method}
	}
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
