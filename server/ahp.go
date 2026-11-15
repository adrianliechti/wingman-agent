package server

// AHP is the shared browser and IDE session transport. The native
// controller remains the owner of execution, command receipts and prompts.
// Lock order is sessionController.mu -> ahpHost.mu; never the reverse.

import (
	"cmp"
	"context"
	"crypto/sha256"
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
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
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
	openMu      sync.Mutex
	terminalMu  sync.Mutex // orders terminal creation: a name is claimed in every project at once
	roots       []string   // directories projects may be opened in
}

// ahpProject is one project's share of a host: agents, sessions, terminals, watches and changesets.
type ahpProject struct {
	*ahpHost
	server       *Server
	dir          string     // the project's identity: its directory with links resolved
	namespace    string     // prefix of the channels this project names itself
	aliasMu      sync.Mutex // orders the saved aliases' writes, which happen outside the host's lock
	used         time.Time  // when a client last reached the project; protected by host.mu
	done         chan struct{}
	stopOnce     sync.Once
	terminalOnce sync.Once
	terminals    map[string]*ahpTerminal
	watches      map[string]*ahpResourceWatch
	watchOnce    sync.Once
	changesMu    sync.Mutex
	changesOnce  sync.Once
	changesets   map[string]*ahpChangeset
}

// ahpAlias maps an AHP URI to the native session behind it. A session's
// alias names its default chat; a forked chat's alias also names the chat.
type ahpAlias struct {
	Resource           string    `json:"resource"`
	Project            string    `json:"project,omitempty"` // directory
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

func newAHPHost() *ahpHost {
	return &ahpHost{creating: map[string]chan struct{}{}, clients: map[*ahpConnection]struct{}{}, sessions: map[string]*ahpSession{}, chats: map[string]*ahpChat{}, aliases: map[string]ahpAlias{}, chatAliases: map[string]ahpAlias{}, versions: map[string]ahpClientVersion{}, projects: map[string]*ahpProject{}}
}

// attach makes a project and its saved sessions reachable through the host.
func (h *ahpHost) attach(s *Server) *ahpProject {
	p := &ahpProject{ahpHost: h, server: s, terminals: map[string]*ahpTerminal{}, namespace: "wingman", used: time.Now(), done: make(chan struct{})}
	if s.workspace != nil {
		p.dir = canonicalPath(s.workspace.RootPath)
	}
	if h.open == nil {
		h.pinned = p
	} else {
		// Several projects share the host's channel names.
		p.namespace += "/" + s.scope.WorkspaceID[:16]
	}
	var saved []ahpAlias
	if dir, err := p.aliasDirectory(); err == nil {
		saved = readAliases(dir)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, alias := range saved {
		alias.Project = p.dir
		h.rememberAliasLocked(alias)
	}
	return p
}

func readAliases(dir string) []ahpAlias {
	var aliases []ahpAlias
	files, _ := os.ReadDir(dir)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		var alias ahpAlias
		if err == nil && json.Unmarshal(data, &alias) == nil && validAHPSessionURI(alias.Resource) && alias.Backend != "" && alias.Session != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// Caller holds h.mu.
func (h *ahpHost) rememberAliasLocked(alias ahpAlias) {
	if alias.Chat != "" {
		h.chatAliases[alias.Chat] = alias
	} else {
		h.aliases[alias.Resource] = alias
	}
}

// ahpHost is the server's project on its host; a workspace server has its own host.
func (s *Server) ahpHost() *ahpProject {
	s.ahpOnce.Do(func() { s.ahp = newAHPHost().attach(s) })
	return s.ahp
}

func (p *ahpProject) aliasDirectory() (string, error) {
	return layout.WingmanPath("ahp", p.server.scope.WorkspaceID)
}

func (p *ahpProject) saveAlias(alias ahpAlias) error {
	dir, err := p.aliasDirectory()
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
	return os.Rename(file.Name(), p.aliasPath(dir, alias))
}

func (h *ahpHost) aliasPath(dir string, alias ahpAlias) string {
	key := alias.Resource
	if alias.Chat != "" {
		key = alias.Chat
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// Caller holds p.mu.
func (p *ahpProject) forgetChatAlias(chat string) {
	alias, ok := p.chatAliases[chat]
	if !ok {
		return
	}
	delete(p.chatAliases, chat)
	p.removeAlias(alias)
}

// Caller holds p.mu.
func (p *ahpProject) forgetAlias(resource string) {
	alias, ok := p.aliases[resource]
	if !ok {
		return
	}
	delete(p.aliases, resource)
	p.removeAlias(alias)
}

func (p *ahpProject) removeAlias(alias ahpAlias) {
	if dir, err := p.aliasDirectory(); err == nil {
		_ = os.Remove(p.aliasPath(dir, alias))
	}
}

// nativeKey names a native session, whose id is unique only within its project and agent.
type nativeKey struct{ project, backend, session string }

func (a ahpAlias) native() nativeKey { return nativeKey{a.Project, a.Backend, a.Session} }

// savedLocked finds the alias of a native session: its forked chat's, or its session's.
func (h *ahpHost) savedLocked(key nativeKey) (ahpAlias, bool) {
	for _, alias := range h.chatAliases {
		if alias.native() == key {
			return alias, true
		}
	}
	for _, alias := range h.aliases {
		if alias.native() == key {
			return alias, true
		}
	}
	return ahpAlias{}, false
}

// storeAlias saves a session's or chat's alias and makes it known to the host.
func (p *ahpProject) storeAlias(alias ahpAlias) error {
	alias.Project = p.dir
	p.aliasMu.Lock()
	defer p.aliasMu.Unlock()
	if err := p.saveAlias(alias); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rememberAliasLocked(alias)
	return nil
}

// creation lets one request at a time create a session or its default chat;
// requests for other URIs, and so every other project, proceed meanwhile.
func (h *ahpHost) creation(session string) (done func()) {
	h.mu.Lock()
	for wait := h.creating[session]; wait != nil; wait = h.creating[session] {
		h.mu.Unlock()
		<-wait
		h.mu.Lock()
	}
	created := make(chan struct{})
	h.creating[session] = created
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.creating, session)
		h.mu.Unlock()
		close(created)
	}
}

func (h *ahpHost) connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) > 0
}

// Only the clients of a workspace server follow one project's files.
func (p *ahpProject) workspaceChanged(f Frame) {
	if p.pinned == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifyRoot("x-wingman/workspaceChanged", map[string]any{"channel": t.RootResourceURI, "event": f})
}

// stop ends the project's own goroutines.
func (p *ahpProject) stop() {
	p.stopOnce.Do(func() { close(p.done) })
}

// detach ends the project. The host of a workspace server ends with it.
func (p *ahpProject) detach() {
	p.stop()
	if p.pinned != nil {
		p.ahpHost.close()
	}
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
	s.ahpHost().serve(s.ctx, w, r)
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
		c.reply(req.ID, t.InitializeResult{ProtocolVersion: version, ServerSeq: h.seq, ServerInfo: &t.Implementation{Name: "wingman", Title: ahpPtr("Wingman")}, Snapshots: snapshots, DefaultDirectory: &dir, Environment: &t.HostEnvironment{PathStyle: &pathStyle}, CompletionTriggerCharacters: []string{"/"}}, nil)
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

// info is the spec's description of the project, which its sessions carry.
func (p *ahpProject) info() *t.ProjectInfo {
	if p.server.workspace == nil {
		return nil
	}
	return projectInfo(p.server.workspace.RootPath)
}

func (p *ahpProject) directoryURI() string {
	root := "/"
	if p.server.workspace != nil {
		root = p.server.workspace.RootPath
	}
	return fileuri.FromPath(root)
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

func startedBackend(projects []*ahpProject, provider string) *backendRuntime {
	for _, p := range projects {
		if runtime := p.startedBackend(provider); runtime != nil {
			return runtime
		}
	}
	return nil
}

func (p *ahpProject) startedBackend(provider string) *backendRuntime {
	p.server.backendsMu.Lock()
	defer p.server.backendsMu.Unlock()
	if entry := p.server.backends[provider]; entry != nil {
		return entry.runtime
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
func (p *ahpProject) backend(provider string) (*backendRuntime, error) {
	b, err := p.server.backend(provider)
	if err == nil {
		return b, nil
	}
	known := provider == code.BuiltinAgentName
	if p.server.workspace != nil && !known {
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
		p, err := h.sessionProject(resource)
		if err != nil {
			return nil, nil, err
		}
		if _, err := p.loadSession(resource, channel != resource); err != nil {
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
	a.project.used = time.Now()
	return a, ch, nil
}

// loadSession loads a session with its chats; a canonical URI whose native
// session is a fork redirects a chat request to that fork's chat.
func (p *ahpProject) loadSession(resource string, chat bool) (*ahpSession, error) {
	if !validAHPSessionURI(resource) {
		return nil, ahpInvalid("unsupported channel")
	}
	p.mu.Lock()
	alias, ok := p.aliases[resource]
	if !ok {
		p.mu.Unlock()
		backend, id, _, err := parseCanonicalURI(resource)
		if err != nil {
			return nil, err
		}
		if resource != p.canonicalURI(backend, id) {
			return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
		}
		alias = ahpAlias{Resource: resource, Project: p.dir, Backend: backend, Session: id}
		p.mu.Lock()
		if other, ok := p.savedLocked(alias.native()); ok {
			p.mu.Unlock()
			if other.Chat != "" && chat {
				return nil, ahpMoved(other.Chat)
			}
			return nil, ahpMoved(other.Resource)
		}
	}
	forks := []ahpAlias{}
	for _, fork := range p.chatAliases {
		if fork.Resource == resource {
			forks = append(forks, fork)
		}
	}
	p.mu.Unlock()
	slices.SortFunc(forks, func(a, b ahpAlias) int { return a.Created.Compare(b.Created) })
	c, title, updated, err := p.loadController(alias.Backend, alias.Session)
	if err != nil {
		return nil, err
	}
	main, err := p.attachSession(c, resource, title, updated)
	if err != nil {
		return nil, err
	}
	a := main.session
	for _, fork := range forks {
		c, _, _, err := p.loadController(fork.Backend, fork.Session)
		var rpc *ahpRPCError
		if errors.As(err, &rpc) && rpc.Code == int(t.ErrorCodeSessionNotFound) {
			p.mu.Lock()
			p.forgetChatAlias(fork.Chat)
			p.mu.Unlock()
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := p.attachChat(a, c, fork); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// loadController loads a native session with its catalog title and time.
func (p *ahpProject) loadController(backend, id string) (*sessionController, string, time.Time, error) {
	b, err := p.backend(backend)
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
func (p *ahpProject) attachSession(c *sessionController, resource, title string, updated time.Time) (*ahpChat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.ahp != nil {
		if c.ahp.session.resource != resource {
			return nil, ahpMoved(c.ahp.session.resource)
		}
		return c.ahp, nil
	}
	a := newAHPSession(p, resource, c.backend.id, title, updated)
	p.sessions[resource] = a
	return a.addChat(c, ahpChatURI(resource), "", nil, time.Time{}), nil
}

func (p *ahpProject) attachChat(a *ahpSession, c *sessionController, alias ahpAlias) (*ahpChat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
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
		return nil, ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Epoch: ch.controller.epoch, Type: "delete"})
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
			if err := ch.command(Command{ID: "ahp-dispose:" + ch.chat.Resource, Epoch: ch.controller.epoch, Type: "delete"}); err != nil {
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

func (p *ahpProject) createSession(raw json.RawMessage, clientID string) (any, error) {
	var params t.CreateSessionParams
	if json.Unmarshal(raw, &params) != nil || !validAHPSessionURI(params.Channel) {
		return nil, ahpInvalid("valid session channel required")
	}
	if strings.HasPrefix(params.Channel, "ahp-session:/wingman/") {
		return nil, ahpInvalid("ahp-session:/wingman/ names existing native sessions")
	}
	if params.ActiveClient != nil && params.ActiveClient.ClientId != clientID {
		return nil, ahpInvalid("active client identity mismatch")
	}
	for _, dir := range params.WorkingDirectories {
		if err := p.validateDirectory(dir); err != nil {
			return nil, err
		}
	}
	provider := code.BuiltinAgentName
	if params.Provider != nil {
		provider = *params.Provider
	}
	defer p.creation(params.Channel)()
	p.mu.Lock()
	alias, exists := p.aliases[params.Channel]
	existing := p.sessions[params.Channel]
	_, chatTaken := p.chatAliases[ahpChatURI(params.Channel)]
	p.mu.Unlock()
	if chatTaken {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionAlreadyExists), Message: "a chat already uses this session's chat URI"}
	}
	requestData, _ := json.Marshal(params)
	requestHash := sha256.Sum256(requestData)
	fingerprint := hex.EncodeToString(requestHash[:])
	if exists && alias.RequestFingerprint != "" && alias.RequestFingerprint != fingerprint {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionAlreadyExists), Message: "session already exists with other creation parameters"}
	}
	if (exists && alias.CreationError == "") || (!exists && existing != nil) {
		return nil, &ahpRPCError{Code: -32003, Message: "session already exists"}
	}
	b, err := p.backend(provider)
	if err != nil {
		return nil, err
	}
	if !b.beginOperation() {
		return nil, errors.New("workspace closed")
	}
	defer b.operations.Done()
	command := Command{ID: "ahp-create:" + params.Channel, Type: "create"}
	if !exists {
		record := b.createSession(command)
		if record.status != 0 {
			return nil, ahpInvalid(record.message)
		}
		alias = ahpAlias{Resource: params.Channel, Backend: provider, Session: record.receipt.Ref.SessionID, Created: time.Now().UTC(), RequestFingerprint: fingerprint, CreationError: "session configuration is incomplete"}
		if err = p.storeAlias(alias); err != nil {
			return nil, err
		}
	}
	id := alias.Session
	c := b.session(id)
	main, err := p.attachSession(c, params.Channel, "", time.Now())
	if err != nil {
		return nil, err
	}
	a := main.session
	if len(params.Config) > 0 {
		if err = main.setConfig("ahp-create-config:"+params.Channel, params.Config); err != nil {
			return nil, err
		}
	}
	alias.CreationError = ""
	if err = p.storeAlias(alias); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if params.ActiveClient != nil {
		a.sessionAction(t.StateAction{Value: &t.SessionActiveClientSetAction{Type: t.ActionTypeSessionActiveClientSet, ActiveClient: *params.ActiveClient}})
	}
	p.notifyRoot("root/sessionAdded", t.SessionAddedParams{Channel: t.RootResourceURI, Summary: a.summary()})
	p.mu.Unlock()
	b.ensureTaskPump(id)
	return nil, nil
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
	// A chat may not take the URI of a session's default chat.
	defer h.creation(strings.Replace(p.Chat, "ahp-chat:", "ahp-session:", 1))()
	compactions := map[string]bool{}
	if fork != nil {
		h.mu.Lock()
		source := a.chat(fork.Chat)
		h.mu.Unlock()
		if source != nil {
			source.controller.mu.Lock()
			for _, turn := range source.controller.hostTurns {
				if turn.compact {
					compactions[turn.entries[0].InputID] = true
				}
			}
			source.controller.mu.Unlock()
		}
	}
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
			// A compaction is no input of the journal; fork through the turn before it.
			for i >= 0 && compactions[source.chat.Turns[i].Id] {
				i--
			}
			if i < 0 {
				h.mu.Unlock()
				return nil, ahpInvalid("fork turn must follow a conversation turn")
			}
			command.InputID = source.chat.Turns[i].Id
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
	project := a.project
	b, err := project.backend(provider)
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
	c, title, _, err := project.loadController(provider, record.receipt.Ref.SessionID)
	if err != nil {
		return nil, err
	}
	alias := ahpAlias{Resource: a.resource, Chat: p.Chat, Title: cmp.Or(command.Title, title), Backend: provider, Session: record.receipt.Ref.SessionID, Created: time.Now().UTC()}
	if fork != nil {
		alias.Fork, alias.Turn = fork.Chat, fork.TurnId
	}
	if err := project.storeAlias(alias); err != nil {
		return nil, err
	}
	if ch, err = project.attachChat(a, c, alias); err != nil {
		return nil, err
	}
	if p.InitialMessage != nil {
		return nil, ch.start(ahpCommandID("initial", p.Chat), *p.InitialMessage)
	}
	return nil, nil
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
	for _, project := range h.loaded() {
		listed, err := project.nativeSessions(root.Agents, seen)
		if err != nil {
			return nil, err
		}
		items = append(items, listed...)
	}
	h.mu.Lock()
	for _, a := range h.sessions {
		if !a.deleted && !seen[a.resource] {
			items = append(items, a.summary())
			seen[a.resource] = true
		}
	}
	// Projects that are not open yet are listed from the sessions saved for them.
	for resource, alias := range h.aliases {
		if h.pinned == nil && !seen[resource] && alias.Project != "" && alias.CreationError == "" && h.projects[alias.Project] == nil {
			items = append(items, h.savedSummaryLocked(alias))
		}
	}
	h.mu.Unlock()
	slices.SortFunc(items, func(a, b t.SessionSummary) int { return strings.Compare(b.ModifiedAt, a.ModifiedAt) })
	// Full retained catalog is returned without a cursor; pagination is optional.
	return t.ListSessionsResult{Items: items}, nil
}

// savedSummaryLocked describes a saved session without loading its project.
func (h *ahpHost) savedSummaryLocked(alias ahpAlias) t.SessionSummary {
	stamp := alias.Created.UTC().Format(time.RFC3339Nano)
	title := cmp.Or(alias.Title, "Session")
	chat := ahpChatURI(alias.Resource)
	return t.SessionSummary{Resource: alias.Resource, Provider: alias.Backend, Title: title, Status: t.SessionStatusIdle, CreatedAt: stamp, ModifiedAt: stamp, Project: projectInfo(alias.Project), WorkingDirectories: []string{fileuri.FromPath(alias.Project)}, DefaultChat: &chat, Chats: h.chatSummariesLocked(alias.Resource, chat, title)}
}

// chatSummariesLocked lists a session's default chat and its saved forks.
func (h *ahpHost) chatSummariesLocked(resource, chat, title string) []t.SessionChatSummary {
	chats := []t.SessionChatSummary{{Resource: chat, Title: title}}
	forks := []ahpAlias{}
	for _, fork := range h.chatAliases {
		if fork.Resource == resource {
			forks = append(forks, fork)
		}
	}
	slices.SortFunc(forks, func(a, b ahpAlias) int { return a.Created.Compare(b.Created) })
	for _, fork := range forks {
		chats = append(chats, t.SessionChatSummary{Resource: fork.Chat, Title: fork.Title})
	}
	return chats
}

// nativeSessions lists the sessions of this project's started agents.
func (p *ahpProject) nativeSessions(agents []t.AgentInfo, seen map[string]bool) ([]t.SessionSummary, error) {
	items := []t.SessionSummary{}
	started := map[string]bool{}
	for _, info := range agents {
		// Listing must not launch every installed external CLI. An external
		// backend becomes available here after first use/config discovery.
		b := p.startedBackend(info.Provider)
		if b == nil || !b.beginOperation() {
			continue
		}
		started[b.id] = true
		sessions, err := b.agent.ListSessions(b.ctx)
		b.operations.Done()
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			resource := p.canonicalURI(b.id, session.ID)
			p.mu.Lock()
			if alias, ok := p.savedLocked(nativeKey{p.dir, b.id, session.ID}); ok {
				// Forked chats are listed with their session.
				if alias.Chat != "" {
					p.mu.Unlock()
					continue
				}
				resource = alias.Resource
			}
			if a := p.sessions[resource]; a != nil {
				if !a.deleted {
					items = append(items, a.summary())
					seen[a.resource] = true
				}
				p.mu.Unlock()
				continue
			}
			stamp := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
			chat := ahpChatURI(resource)
			chats := p.chatSummariesLocked(resource, chat, session.Title)
			p.mu.Unlock()
			items = append(items, t.SessionSummary{Resource: resource, Provider: b.id, Title: session.Title, Status: t.SessionStatusIdle, CreatedAt: stamp, ModifiedAt: stamp, Project: p.info(), WorkingDirectories: []string{p.directoryURI()}, DefaultChat: &chat, Chats: chats})
			seen[resource] = true
		}
	}
	// An agent that has not started cannot list its sessions; the saved ones stay listed.
	p.mu.Lock()
	defer p.mu.Unlock()
	for resource, alias := range p.aliases {
		if p.pinned == nil && alias.Project == p.dir && !started[alias.Backend] && !seen[resource] && alias.CreationError == "" && p.sessions[resource] == nil {
			items = append(items, p.savedSummaryLocked(alias))
			seen[resource] = true
		}
	}
	return items, nil
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
