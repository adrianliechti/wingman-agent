package server

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

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
	if h.closed {
		h.mu.Unlock()
		return nil, nil, errHostClosed
	}
	a, _ := h.channel(channel)
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
	if h.closed {
		return nil, nil, errHostClosed
	}
	a, ch := h.channel(channel)
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
	if ok && alias.CreationError != "" {
		p.mu.Unlock()
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
	}
	if !ok {
		p.mu.Unlock()
		backend, id, dir, err := parseCanonicalURI(resource)
		if err != nil {
			return nil, err
		}
		if canonical := p.canonicalURI(backend, id); resource != canonical {
			if dir != "" {
				return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
			}
			if chat {
				return nil, ahpMoved(ahpChatURI(canonical))
			}
			return nil, ahpMoved(canonical)
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
	forks := p.forksLocked(resource)
	p.mu.Unlock()
	c, title, updated, err := p.loadController(alias.Backend, alias.Session)
	if err != nil {
		return nil, err
	}
	// The saved title holds a client's rename and the title the host gave the session.
	main, err := p.attachSession(c, resource, cmp.Or(alias.Title, title), updated)
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
		// A chat that does not load stays saved; its session opens without it.
		if err == nil {
			_, _ = p.attachChat(a, c, fork)
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
		if errors.Is(err, os.ErrNotExist) || err.Error() == "session not found" || err.Error() == "session deleted" {
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
	if !p.availableLocked() {
		return nil, errHostClosed
	}
	if c.ahp != nil {
		if c.ahp.session.resource != resource {
			return nil, ahpMoved(c.ahp.session.resource)
		}
		return c.ahp, nil
	}
	alias := p.aliases[resource]
	if alias.Modified.After(updated) {
		updated = alias.Modified
	}
	a := newAHPSession(p, resource, c.backend.id, title, cmp.Or(alias.Created, updated), updated, alias.Status)
	p.sessions[resource] = a
	return a.addChat(c, ahpChatURI(resource), "", nil, time.Time{}), nil
}

func (p *ahpProject) attachChat(a *ahpSession, c *sessionController, alias ahpAlias) (*ahpChat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.availableLocked() {
		return nil, errHostClosed
	}
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
	if _, err := ahpSettings(params.Config); err != nil {
		return nil, err
	}
	provider := code.BuiltinAgentName
	if params.Provider != nil {
		provider = *params.Provider
	}
	defer p.creation(params.Channel)()
	p.mu.Lock()
	alias, exists := p.aliases[params.Channel]
	_, chatTaken := p.chatAliases[ahpChatURI(params.Channel)]
	p.mu.Unlock()
	if chatTaken {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeSessionAlreadyExists), Message: "a chat already uses this session's chat URI"}
	}
	if exists && alias.CreationError == "" {
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
	if exists {
		// A creation that a restart cut off starts over.
		p.discard(b.session(alias.Session), alias)
	}
	record := b.createSession(Command{ID: "ahp-create:" + params.Channel, Type: "create"})
	if record.status != 0 {
		return nil, ahpInvalid(record.message)
	}
	id := record.receipt.Ref.SessionID
	c := b.session(id)
	// Until creation completes, the saved session names an incomplete one that a restart discards.
	alias = ahpAlias{Resource: params.Channel, Backend: provider, Session: id, Created: time.Now().UTC(), CreationError: "session configuration is incomplete"}
	fail := func(err error) (any, error) {
		p.discard(c, alias)
		return nil, err
	}
	if err = p.storeAlias(alias); err != nil {
		return fail(err)
	}
	main, err := p.attachSession(c, params.Channel, "", time.Now())
	if err != nil {
		return fail(err)
	}
	a := main.session
	if len(params.Config) > 0 {
		if err = a.configure("ahp-create-config:"+params.Channel, params.Config); err != nil {
			return fail(err)
		}
	}
	alias.CreationError = ""
	if err = p.storeAlias(alias); err != nil {
		return fail(err)
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
	alias := ahpAlias{Resource: a.resource, Chat: p.Chat, Backend: provider, Session: record.receipt.Ref.SessionID, Created: time.Now().UTC()}
	if fork != nil {
		alias.Fork, alias.Turn = fork.Chat, fork.TurnId
	}
	c, title, _, err := project.loadController(provider, alias.Session)
	if err != nil {
		project.discard(b.session(alias.Session), alias)
		return nil, err
	}
	alias.Title = cmp.Or(command.Title, title)
	if err := project.storeAlias(alias); err != nil {
		project.discard(c, alias)
		return nil, err
	}
	// Disposing the session meanwhile leaves the chat nothing to join.
	if ch, err = project.attachChat(a, c, alias); err != nil {
		project.discard(c, alias)
		return nil, err
	}
	// A new chat sets nothing of its own, and a fork what its source set; a value its backend refuses shows as the chat's.
	_ = ch.syncSettings(ahpCommandID("chat-config", p.Chat))
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
		if !seen[a.resource] {
			items = append(items, a.summary())
			seen[a.resource] = true
		}
	}
	// Projects that are not open yet are listed from the sessions saved for them.
	for resource, alias := range h.aliases {
		if !seen[resource] && alias.Project != "" && alias.CreationError == "" && h.projects[alias.Project] == nil {
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
	created := alias.Created.UTC().Format(time.RFC3339Nano)
	modified := cmp.Or(alias.Modified, alias.Created).UTC().Format(time.RFC3339Nano)
	title := cmp.Or(alias.Title, "Session")
	chat := ahpChatURI(alias.Resource)
	return t.SessionSummary{Resource: alias.Resource, Provider: alias.Backend, Title: title, Status: t.SessionStatusIdle | alias.Status, CreatedAt: created, ModifiedAt: modified, Project: projectInfo(alias.Project), WorkingDirectories: []string{fileuri.FromPath(alias.Project)}, DefaultChat: &chat, Chats: h.chatSummariesLocked(alias.Resource, chat)}
}

// chatSummariesLocked lists a session's default chat and its saved forks.
func (h *ahpHost) chatSummariesLocked(resource, chat string) []t.SessionChatSummary {
	chats := []t.SessionChatSummary{{Resource: chat}} // the default chat inherits the session's title
	for _, fork := range h.forksLocked(resource) {
		chats = append(chats, t.SessionChatSummary{Resource: fork.Chat, Title: fork.Title})
	}
	return chats
}

// nativeSessions lists the sessions of this project's started agents.
func (p *ahpProject) nativeSessions(agents []t.AgentInfo, seen map[string]bool) ([]t.SessionSummary, error) {
	items := []t.SessionSummary{}
	for _, info := range agents {
		// Listing must not launch every installed external CLI. An external
		// backend becomes available here after first use/config discovery.
		b := p.startedBackend(info.Provider)
		if b == nil || !b.beginOperation() {
			continue
		}
		sessions, err := b.agent.ListSessions(b.ctx)
		b.operations.Done()
		// An agent that cannot list its sessions has its saved ones listed.
		if err != nil {
			continue
		}
		for _, session := range sessions {
			resource := p.canonicalURI(b.id, session.ID)
			p.mu.Lock()
			alias, saved := p.savedLocked(nativeKey{p.dir, b.id, session.ID})
			if saved {
				// Forked chats are listed with their session.
				if alias.Chat != "" {
					p.mu.Unlock()
					continue
				}
				resource = alias.Resource
			}
			if a := p.sessions[resource]; a != nil {
				items = append(items, a.summary())
				seen[a.resource] = true
				p.mu.Unlock()
				continue
			}
			if saved {
				if alias.CreationError == "" {
					if session.UpdatedAt.After(alias.Modified) {
						alias.Modified = session.UpdatedAt
					}
					items = append(items, p.savedSummaryLocked(alias))
					seen[resource] = true
				}
				p.mu.Unlock()
				continue
			}
			stamp := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
			chat := ahpChatURI(resource)
			chats := p.chatSummariesLocked(resource, chat)
			p.mu.Unlock()
			items = append(items, t.SessionSummary{Resource: resource, Provider: b.id, Title: session.Title, Status: t.SessionStatusIdle, CreatedAt: stamp, ModifiedAt: stamp, Project: p.info(), WorkingDirectories: []string{p.directoryURI()}, DefaultChat: &chat, Chats: chats})
			seen[resource] = true
		}
	}
	// Saved sessions its agents do not list, such as unused ones or those of an agent that has not started, stay listed.
	p.mu.Lock()
	defer p.mu.Unlock()
	for resource, alias := range p.aliases {
		if alias.Project == p.dir && !seen[resource] && alias.CreationError == "" && p.sessions[resource] == nil {
			items = append(items, p.savedSummaryLocked(alias))
			seen[resource] = true
		}
	}
	return items, nil
}

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

// canonicalURI names a native session that no client named; the host finds it by its project.
func (p *ahpProject) canonicalURI(backend, id string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return "ahp-session:/wingman/" + encode([]byte(backend)) + "/" + encode([]byte(id)) + "/" + encode([]byte(p.dir))
}

func parseCanonicalURI(resource string) (backend, id, dir string, err error) {
	parts := strings.Split(strings.TrimPrefix(resource, "ahp-session:/wingman/"), "/")
	if !strings.HasPrefix(resource, "ahp-session:/wingman/") || len(parts) != 2 && len(parts) != 3 {
		return "", "", "", &ahpRPCError{Code: int(t.ErrorCodeSessionNotFound), Message: "unknown session"}
	}
	values := make([]string, 3)
	for i, part := range parts {
		data, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(data) == 0 {
			return "", "", "", ahpInvalid("invalid session URI")
		}
		values[i] = string(data)
	}
	return values[0], values[1], values[2], nil
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
