package server

// AHP is the shared browser and IDE session transport. The native
// controller remains the owner of execution, command receipts and prompts.
// Lock order is sessionController.mu -> Host.mu; never the reverse.
// Project startup and teardown hold Host.projectMu, taking mu only to
// change the registry. Backend I/O and resource cleanup happen outside mu.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/model"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/coder/websocket"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// A client's negotiated version outlives its socket briefly, so a reconnect
// keeps the projection it negotiated.
type clientVersion struct {
	version string
	left    time.Time // zero while a connection with this identity is open
}

const clientVersionTTL = 10 * time.Minute

// hostClient owns terminals that no connected client claims.
const hostClient = "wingman-host"

type connection struct {
	ctx           context.Context
	host          *Host
	writer        *wsClient
	id            string
	version       string
	subscriptions map[string]bool // protected by host.mu
	lanes         map[string]chan ahp.Request
	requests      chan struct{}
}

func (h *Host) connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// serve runs one client connection until it closes or the host ends.
func (h *Host) serve(w http.ResponseWriter, r *http.Request) {
	if !ahp.OriginAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	c := &connection{host: h, writer: newWSClient(conn), subscriptions: map[string]bool{}, ctx: ctx, lanes: map[string]chan ahp.Request{}, requests: make(chan struct{}, 16)}
	h.mu.Lock()
	if h.ctx.Err() != nil {
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
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		req, err := ahp.Decode(data)
		if err != nil {
			c.reply(req.ID, nil, err)
			continue
		}
		if req.Method == "dispatchAction" || req.Method == "unsubscribe" {
			if len(req.ID) != 0 {
				c.reply(req.ID, nil, ahp.InvalidParams("method must be a notification"))
				continue
			}
			if c.id != "" {
				c.enqueue(req)
			}
			continue
		}
		if c.id == "" && req.Method != "initialize" && req.Method != "reconnect" && req.Method != "ping" {
			if len(req.ID) != 0 {
				c.reply(req.ID, nil, ahp.InvalidParams("initialize first"))
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
				var rpc *ahp.Error
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
		if req.Method == "subscribe" {
			c.enqueue(req)
			continue
		}
		select {
		case c.requests <- struct{}{}:
		default:
			c.reply(req.ID, nil, &ahp.Error{Code: -32603, Message: "too many concurrent requests"})
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
// Only the connection read loop owns the lane map and enqueues requests.
func (c *connection) enqueue(req ahp.Request) {
	var p struct {
		Channel string `json:"channel"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.Channel == "" {
		c.reply(req.ID, nil, ahp.InvalidParams("channel required"))
		return
	}
	lane := c.lanes[p.Channel]
	if lane == nil {
		if len(c.lanes) >= 256 {
			c.reply(req.ID, nil, ahp.InvalidParams("too many channels"))
			return
		}
		lane = make(chan ahp.Request, 1024)
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
	select {
	case lane <- req:
	default:
		c.writer.close() // a client that floods one channel loses its connection, not its order
	}
}

func (c *connection) channelRequest(req ahp.Request) {
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

func (c *connection) reply(id json.RawMessage, result any, err error) {
	c.writer.deliver(ahp.Reply(id, result, err))
}

func (c *connection) notify(method string, params any) {
	c.writer.deliver(ahp.Notify(method, params))
}

func (h *Host) notifyRoot(method string, params any) {
	for c := range h.clients {
		if c.subscriptions[types.RootResourceURI] {
			c.notify(method, params)
		}
	}
}

// A reconnect may overlap the old socket closing. Keep the client's session
// presence while another connection with that identity is subscribed.
func (h *Host) removeDetachedClient(a *Session, id string) {
	for c := range h.clients {
		if c.id == id && c.subscriptions[a.resource] {
			return
		}
	}
	a.removeClient(id)
}

func (h *Host) publishAgents() {
	root, err := h.root()
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return
	}
	h.publish(types.RootResourceURI, types.StateAction{Value: &types.RootAgentsChangedAction{Type: types.ActionTypeRootAgentsChanged, Agents: root.Agents}}, nil, nil)
}

func (h *Host) publish(channel string, action types.StateAction, origin *types.ActionOrigin, rejection *string) types.ActionEnvelope {
	// 1.0 reducers keep queued and steering input in separate slots. Clear the
	// opposite slot before a kind change, without changing 1.1's stable ordering.
	if set, ok := action.Value.(*types.ChatPendingMessageSetAction); ok && rejection == nil {
		h.seq++
		kind := types.PendingMessageKindQueued
		if set.Kind == kind {
			kind = types.PendingMessageKindSteering
		}
		remove := types.StateAction{Value: &types.ChatPendingMessageRemovedAction{Type: types.ActionTypeChatPendingMessageRemoved, Kind: kind, Id: set.Id}}
		for c := range h.clients {
			if c.legacyQueue() && c.subscriptions[channel] {
				c.notify("action", types.ActionEnvelope{Channel: channel, Action: remove, ServerSeq: h.seq})
			}
		}
	}
	h.seq++
	envelope := types.ActionEnvelope{Channel: channel, Action: action, ServerSeq: h.seq, Origin: origin, RejectionReason: rejection}
	for c := range h.clients {
		if c.subscriptions[channel] || origin != nil && origin.ClientId == c.id {
			c.notify("action", envelope)
		}
	}
	return envelope
}

func (c *connection) legacyQueue() bool {
	return c.version == "" || strings.HasPrefix(c.version, "0.") || strings.HasPrefix(c.version, "1.0.")
}

func (c *connection) chatSnapshot(chat *types.ChatState) *types.ChatState {
	if !c.legacyQueue() {
		return chat
	}
	state := *chat
	state.QueuedMessages = nil
	for _, message := range chat.QueuedMessages {
		if message.Kind == nil || *message.Kind == types.PendingMessageKindQueued {
			state.QueuedMessages = append(state.QueuedMessages, message)
		}
	}
	return &state
}

func (c *connection) subscribeRequest(req ahp.Request) error {
	h := c.host
	var channels []string
	var clientID, version string
	switch req.Method {
	case "initialize":
		if c.id != "" {
			return ahp.InvalidParams("already initialized")
		}
		var p types.InitializeParams
		if json.Unmarshal(req.Params, &p) != nil || p.Channel != types.RootResourceURI || p.ClientId == "" || p.ClientId == hostClient {
			return ahp.InvalidParams("root channel and clientId required")
		}
		var err error
		version, err = types.NegotiateProtocolVersion(p.ProtocolVersions)
		if err != nil {
			return ahp.InvalidParams(err.Error())
		}
		if version == "" {
			return &ahp.Error{Code: -32005, Message: "unsupported protocol version", Data: map[string]any{"supportedVersions": types.SupportedProtocolVersions()}}
		}
		clientID, channels = p.ClientId, p.InitialSubscriptions
	case "reconnect":
		if c.id != "" {
			return ahp.InvalidParams("already initialized")
		}
		var p types.ReconnectParams
		if json.Unmarshal(req.Params, &p) != nil || p.Channel != types.RootResourceURI || p.ClientId == "" || p.ClientId == hostClient {
			return ahp.InvalidParams("root channel and clientId required")
		}
		clientID, channels = p.ClientId, p.Subscriptions
	case "subscribe":
		if c.id == "" {
			return ahp.InvalidParams("initialize first")
		}
		var p types.SubscribeParams
		if json.Unmarshal(req.Params, &p) != nil || p.Channel == "" {
			return ahp.InvalidParams("channel required")
		}
		channels = []string{p.Channel}
	}
	if len(channels) > 128 {
		return ahp.InvalidParams("too many subscriptions")
	}
	var root *types.RootState
	for _, channel := range channels {
		if channel == types.RootResourceURI {
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
				return &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "unknown client; initialize again"}
			}
			version = known.version
		}
		h.pruneVersionsLocked()
		h.versions[clientID] = clientVersion{version: version}
		c.id, c.version = clientID, version
	}
	if len(c.subscriptions)+len(channels) > 128 {
		return ahp.InvalidParams("too many subscriptions")
	}
	snapshots := []types.Snapshot{}
	for _, channel := range channels {
		var state types.SnapshotState
		if channel == types.RootResourceURI {
			root.Terminals = h.terminalsLocked()
			state.Root = root
		} else if p := h.ownerLocked(channel); p != nil && p.snapshotLocked(channel, &state) {
		} else {
			a, ch := h.channel(channel)
			if a == nil || ch == nil && channel != a.resource {
				if req.Method == "reconnect" {
					continue
				}
				return &ahp.Error{Code: int(types.ErrorCodeSessionNotFound), Message: "unknown channel"}
			}
			if ch == nil {
				state.Session = &a.state
			} else {
				state.Chat = c.chatSnapshot(&ch.chat)
			}
		}
		c.subscriptions[channel] = true
		snapshots = append(snapshots, types.Snapshot{Resource: channel, State: state, FromSeq: h.seq})
	}
	switch req.Method {
	case "initialize":
		dir := h.defaultDirectoryLocked()
		pathStyle := types.PathStylePosix
		if runtime.GOOS == "windows" {
			pathStyle = types.PathStyleWindows
		}
		c.reply(req.ID, types.InitializeResult{ProtocolVersion: version, ServerSeq: h.seq, ServerInfo: &types.Implementation{Name: "wingman", Title: new("Wingman")}, Snapshots: snapshots, DefaultDirectory: &dir, Environment: &types.HostEnvironment{PathStyle: &pathStyle}, CompletionTriggerCharacters: []string{"/"}}, nil)
	case "reconnect":
		// AHP explicitly permits snapshot recovery when replay is unavailable.
		c.reply(req.ID, types.ReconnectResult{Value: &types.ReconnectSnapshotResult{Snapshots: snapshots}}, nil)
	case "subscribe":
		c.reply(req.ID, types.SubscribeResult{Snapshot: &snapshots[0]}, nil)
	}
	return nil
}

func (h *Host) clientLeftLocked(id string) {
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

func (h *Host) pruneVersionsLocked() {
	for id, known := range h.versions {
		if !known.left.IsZero() && time.Since(known.left) > clientVersionTTL {
			delete(h.versions, id)
		}
	}
}

// Models are known only for agents that a project has started.
func (h *Host) root() (types.RootState, error) {
	h.mu.Lock()
	projects, web := h.projectsLocked(), h.web
	h.mu.Unlock()
	for _, p := range projects {
		p.startTerminals()
	}
	// A project without a workspace (tests) offers only the agents it runs.
	var available []AgentEntry
	if web == nil || web.workspace != nil {
		available = append(available, AgentEntry{ID: code.BuiltinAgentName, Name: "Wingman"})
		external, err := agents.Available()
		if err != nil {
			return types.RootState{}, err
		}
		for _, a := range external {
			available = append(available, AgentEntry{ID: a.ID, Name: a.Name})
		}
	}
	// Running backends are listed even when no registration names them.
	for _, p := range projects {
		p.backendsMu.Lock()
		for id := range p.backends {
			if !slices.ContainsFunc(available, func(entry AgentEntry) bool { return entry.ID == id }) {
				available = append(available, AgentEntry{ID: id, Name: id})
			}
		}
		p.backendsMu.Unlock()
	}
	// VS Code reads this optional metadata to interpret remote file paths
	// without falling back to its private network diagnostics extension.
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	resources, _ := json.Marshal(map[string]any{"platform": platform, "architecture": runtime.GOARCH, "cpuCount": runtime.NumCPU()})
	root := types.RootState{Agents: []types.AgentInfo{}, Meta: map[string]json.RawMessage{"vscode.agentHost.resources": resources}}
	if web != nil {
		scope := web.scope
		workspace, _ := json.Marshal(map[string]any{"workspaceId": scope.WorkspaceID, "instanceId": scope.InstanceID})
		root.Meta["wingman.workspace"] = workspace
	}
	for _, entry := range available {
		info := types.AgentInfo{Provider: entry.ID, DisplayName: entry.Name, Description: entry.Name + " in Wingman", Models: []types.SessionModelInfo{}, Capabilities: &types.AgentCapabilities{}}
		runtime := startedBackend(projects, entry.ID)
		if runtime != nil {
			if _, ok := runtime.agent.(code.SessionForker); ok {
				info.Capabilities.MultipleChats = &types.MultipleChatsCapability{Fork: new(true)}
			}
		}
		if runtime != nil && runtime.beginOperation() {
			models, _ := runtime.agent.Models("")
			for _, m := range models {
				info.Models = append(info.Models, modelInfo(entry.ID, m))
			}
			runtime.operations.Done()
		}
		root.Agents = append(root.Agents, info)
	}
	return root, nil
}

func startedBackend(projects []*Project, provider string) *backendRuntime {
	for _, p := range projects {
		if runtime := p.startedBackend(provider); runtime != nil {
			return runtime
		}
	}
	return nil
}

// defaultDirectoryLocked is where a client starts choosing a working directory.
// Caller holds h.mu.
func (h *Host) defaultDirectoryLocked() string {
	if h.web != nil {
		return h.web.directoryURI()
	}
	return fileuri.FromPath(h.roots[0])
}

func modelInfo(provider string, m model.Model) types.SessionModelInfo {
	info := types.SessionModelInfo{Id: m.ID, Name: m.Name, Provider: provider}
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
		effort := types.ConfigPropertySchema{Type: "string", Title: "Reasoning effort"}
		for _, value := range m.Efforts {
			data, _ := json.Marshal(value)
			effort.Enum = append(effort.Enum, data)
		}
		if m.Effort != "" {
			data, _ := json.Marshal(m.Effort)
			effort.Default = new(json.RawMessage(data))
		}
		info.ConfigSchema = &types.ConfigSchema{Type: "object", Properties: map[string]types.ConfigPropertySchema{"effort": effort}}
	}
	return info
}

func (c *connection) request(req ahp.Request) (any, error) {
	if req.Method == "ping" {
		return nil, nil
	}
	if c.id == "" {
		return nil, ahp.InvalidParams("initialize first")
	}
	h := c.host
	var base struct {
		Channel string `json:"channel"`
	}
	if json.Unmarshal(req.Params, &base) != nil || base.Channel == "" {
		return nil, ahp.InvalidParams("channel required")
	}
	switch req.Method {
	case "invokeChangesetOperation":
		p := h.owner(base.Channel)
		if p == nil {
			return nil, ahp.InvalidParams("changeset has no operations")
		}
		return p.invokeChangesetOperation(req.Params)
	case "createTerminal":
		return c.createTerminal(req.Params)
	case "createResourceWatch":
		var watch types.CreateResourceWatchParams
		if json.Unmarshal(req.Params, &watch) != nil || watch.Channel != types.RootResourceURI {
			return nil, ahp.InvalidParams("root channel required")
		}
		p, err := h.loadedProject(&watch.Uri)
		if err != nil {
			return nil, err
		}
		return p.createResourceWatch(req.Params)
	case "disposeTerminal":
		return c.disposeTerminal(base.Channel)
	case "listSessions":
		if base.Channel != types.RootResourceURI {
			return nil, ahp.InvalidParams("root channel required")
		}
		return h.listSessions(req.Params)
	case "createSession":
		var session types.CreateSessionParams
		if json.Unmarshal(req.Params, &session) != nil {
			return nil, ahp.InvalidParams("valid session channel required")
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
			return nil, ahp.InvalidParams("only forked chats can be disposed; dispose the session instead")
		}
		return nil, ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Type: "delete"})
	case "resolveSessionConfig":
		var p types.ResolveSessionConfigParams
		if json.Unmarshal(req.Params, &p) != nil {
			return nil, ahp.InvalidParams("invalid session config")
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
		cfg := configFromSettings(b.settings(""))
		// An agent that just started may know its models.
		if !started {
			h.publishAgents()
		}
		for key, value := range p.Config {
			if _, ok := cfg.Schema.Properties[key]; ok {
				cfg.Values[key] = value
			}
		}
		return types.ResolveSessionConfigResult{Schema: cfg.Schema, Values: cfg.Values}, nil
	case "sessionConfigCompletions":
		return types.SessionConfigCompletionsResult{Items: []types.SessionConfigValueItem{}}, nil
	case "completions":
		return h.completions(req.Params)
	case "fetchTurns":
		var p types.FetchTurnsParams
		if json.Unmarshal(req.Params, &p) != nil || p.Cursor != nil {
			return nil, ahp.InvalidParams("unknown history cursor")
		}
		_, _, err := h.ensureSession(base.Channel)
		return struct{}{}, err
	case "disposeSession":
		a, ch, err := h.ensureSession(base.Channel)
		if err != nil {
			return nil, err
		}
		if ch != nil {
			return nil, ahp.InvalidParams("session channel required")
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
		return result, resourceError(err)
	default:
		return nil, &ahp.Error{Code: -32601, Message: "method not supported: " + req.Method}
	}
}

func (c *connection) unsubscribe(raw json.RawMessage) {
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
