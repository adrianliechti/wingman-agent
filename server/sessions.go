package server

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// channel finds a loaded session or chat. Caller holds h.mu.
func (h *Host) channel(channel string) (*Session, *Chat) {
	if ch := h.chats[channel]; ch != nil {
		return ch.session, ch
	}
	return h.sessions[channel], nil
}

// sessionURI names the session a session or chat channel belongs to. Caller holds h.mu.
func (h *Host) sessionURI(channel string) string {
	if alias, ok := h.chatAliases[channel]; ok {
		return alias.Resource
	}
	return ahp.SessionURI(channel)
}

// moved tells a client that a canonical URI's session lives under an alias.
func moved(resource string) error {
	return &ahp.Error{Code: int(types.ErrorCodeInvalidParams), Message: "session is available at " + resource, Data: map[string]string{"resource": resource}}
}

// ensureSession loads the session behind a session or chat channel. The chat
// is nil for the session channel itself.
func (h *Host) ensureSession(channel string) (*Session, *Chat, error) {
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		return nil, nil, errHostClosed
	}
	a, _ := h.channel(channel)
	resource := h.sessionURI(channel)
	h.mu.Unlock()
	if a == nil {
		// Cold loads and deletion share the same gate as creation. A load
		// cannot attach a controller after its native session was deleted.
		defer h.creation(resource)()
		h.mu.Lock()
		a, _ = h.channel(channel)
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
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return nil, nil, errHostClosed
	}
	a, ch := h.channel(channel)
	if a == nil {
		return nil, nil, &ahp.Error{Code: int(types.ErrorCodeSessionNotFound), Message: "session disposed"}
	}
	if ch == nil && channel != a.resource {
		return nil, nil, &ahp.Error{Code: int(types.ErrorCodeSessionNotFound), Message: "unknown chat"}
	}
	a.project.used = time.Now()
	return a, ch, nil
}

// loadSession loads a session with its chats; a canonical URI whose native
// session is a fork redirects a chat request to that fork's chat.
func (p *Project) loadSession(resource string, chat bool) (*Session, error) {
	if !ahp.ValidSessionURI(resource) {
		return nil, ahp.InvalidParams("unsupported channel")
	}
	p.host.mu.Lock()
	alias, ok := p.host.aliases[resource]
	if ok && alias.CreationError != "" {
		p.host.mu.Unlock()
		return nil, errUnknownSession
	}
	if !ok {
		p.host.mu.Unlock()
		backend, id, dir, err := parseCanonicalURI(resource)
		if err != nil {
			return nil, err
		}
		if canonical := p.canonicalURI(backend, id); resource != canonical {
			if dir != "" {
				return nil, errUnknownSession
			}
			if chat {
				return nil, moved(ahp.ChatURI(canonical))
			}
			return nil, moved(canonical)
		}
		alias = sessionAlias{Project: p.dir, Backend: backend, Session: id}
		key := nativeSessionKey(p.dir, backend, id)
		p.host.mu.Lock()
		if other, ok := p.host.savedIndexLocked()[key]; ok {
			p.host.mu.Unlock()
			if other.Chat != "" && chat {
				return nil, moved(other.Chat)
			}
			return nil, moved(other.Resource)
		}
	}
	forks := p.host.forksLocked(resource)
	p.host.mu.Unlock()
	c, err := p.loadController(alias)
	if err != nil {
		// A session its agent no longer has leaves the list.
		if errors.Is(err, errUnknownSession) {
			p.host.mu.Lock()
			p.host.forgetSessionLocked(resource)
			p.host.mu.Unlock()
			p.host.saveAliases()
		}
		return nil, err
	}
	title, updated := c.backend.listing(alias.Session)
	// The saved title holds a client's rename and the title the host gave the session.
	main, err := p.attachSession(c, resource, cmp.Or(alias.Title, title), updated)
	if err != nil {
		return nil, err
	}
	a := main.session
	for _, fork := range forks {
		c, err := p.loadController(fork)
		var rpc *ahp.Error
		if errors.As(err, &rpc) && rpc.Code == int(types.ErrorCodeSessionNotFound) {
			p.host.mu.Lock()
			p.host.forgetChatAliasLocked(fork.Chat)
			p.host.mu.Unlock()
			p.host.saveAliases()
			continue
		}
		// A chat that does not load stays saved; its session opens without it.
		if err == nil {
			_, _ = p.attachChat(a, c, fork)
		}
	}
	return a, nil
}

// loadController loads a native session. Global agents can resume IDs from
// another folder, so a session without a saved alias must match their listing.
func (p *Project) loadController(alias sessionAlias) (*sessionController, error) {
	id := alias.Session
	b, err := p.backend(alias.Backend)
	if err != nil {
		return nil, err
	}
	if _, global := everyFolderLister(b.agent); global && alias.Resource == "" {
		if _, err := p.host.listedSession(alias.Backend, id, p.dir); err != nil {
			return nil, err
		}
	}
	if !b.Project.beginOperation() {
		return nil, errWorkspaceClosed
	}
	defer b.Project.operations.Done()
	c := b.session(id)
	c.opMu.Lock()
	err = c.loadLocked()
	c.opMu.Unlock()
	if err != nil {
		b.discard(id, c)
		if sessionGone(err) || errors.Is(err, errSessionDeleted) {
			return nil, errUnknownSession
		}
		return nil, err
	}
	return c, nil
}

// listSessions asks the agent for the sessions of its project's folder.
func (b *backendRuntime) listSessions() ([]code.SessionInfo, error) {
	if !b.Project.beginOperation() {
		return nil, errWorkspaceClosed
	}
	defer b.Project.operations.Done()
	return b.agent.ListSessions(b.Project.ctx)
}

// listing is a native session's title and last change as its agent lists them.
func (b *backendRuntime) listing(id string) (string, time.Time) {
	// Attaching must not make an old session look recently modified.
	title, updated := "", time.Now()
	if sessions, err := b.listSessions(); err == nil {
		if i := slices.IndexFunc(sessions, func(info code.SessionInfo) bool { return info.ID == id }); i >= 0 {
			title, updated = sessions[i].Title, sessions[i].UpdatedAt
		}
	}
	return title, updated
}

// attachSession projects a native session as an AHP session and returns its default chat.
func (p *Project) attachSession(c *sessionController, resource, title string, updated time.Time) (*Chat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if !p.availableLocked() {
		return nil, errHostClosed
	}
	if c.chat != nil {
		if c.chat.session.resource != resource {
			return nil, moved(c.chat.session.resource)
		}
		return c.chat, nil
	}
	alias := p.host.aliases[resource]
	if alias.Modified.After(updated) {
		updated = alias.Modified
	}
	a := newSession(p, resource, c.backend.id, title, cmp.Or(alias.Created, updated), updated, alias.flags())
	p.host.sessions[resource] = a
	return a.addChat(c, ahp.ChatURI(resource), "", nil, time.Time{}), nil
}

func (p *Project) attachChat(a *Session, c *sessionController, alias sessionAlias) (*Chat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if !p.availableLocked() {
		return nil, errHostClosed
	}
	if p.host.sessions[a.resource] != a {
		return nil, &ahp.Error{Code: int(types.ErrorCodeSessionNotFound), Message: "session disposed"}
	}
	if c.chat != nil {
		if c.chat.chat.Resource != alias.Chat {
			return nil, ahp.InvalidParams("native session is already a chat elsewhere")
		}
		return c.chat, nil
	}
	var origin *types.ChatOrigin
	if alias.Fork != "" {
		origin = &types.ChatOrigin{Value: &types.ChatForkOrigin{Kind: types.ChatOriginKindFork, Chat: alias.Fork, TurnId: alias.Turn}}
	}
	return a.addChat(c, alias.Chat, alias.Title, origin, alias.Created), nil
}

func (p *Project) createSession(raw json.RawMessage, clientID string) (any, error) {
	var params types.CreateSessionParams
	if json.Unmarshal(raw, &params) != nil || !ahp.ValidSessionURI(params.Channel) {
		return nil, ahp.InvalidParams("valid session channel required")
	}
	if strings.HasPrefix(params.Channel, "ahp-session:/wingman/") {
		return nil, ahp.InvalidParams("ahp-session:/wingman/ names existing native sessions")
	}
	if params.ActiveClient != nil && params.ActiveClient.ClientId != clientID {
		return nil, ahp.InvalidParams("active client identity mismatch")
	}
	for _, dir := range params.WorkingDirectories {
		if err := p.validateDirectory(dir); err != nil {
			return nil, err
		}
	}
	if _, err := settingsFromConfig(params.Config); err != nil {
		return nil, err
	}
	provider := code.BuiltinAgentName
	if params.Provider != nil {
		provider = *params.Provider
	}
	defer p.host.creation(params.Channel)()
	p.host.mu.Lock()
	alias, exists := p.host.aliases[params.Channel]
	_, chatTaken := p.host.chatAliases[ahp.ChatURI(params.Channel)]
	p.host.mu.Unlock()
	if chatTaken {
		return nil, &ahp.Error{Code: int(types.ErrorCodeSessionAlreadyExists), Message: "a chat already uses this session's chat URI"}
	}
	if exists && alias.CreationError == "" {
		return nil, &ahp.Error{Code: int(types.ErrorCodeSessionAlreadyExists), Message: "session already exists"}
	}
	// A creation that a restart cut off starts over.
	if exists {
		if err := p.host.discardSaved(alias); err != nil {
			return nil, err
		}
	}
	b, err := p.backend(provider)
	if err != nil {
		return nil, err
	}
	if !b.Project.beginOperation() {
		return nil, errWorkspaceClosed
	}
	defer b.Project.operations.Done()
	record := b.createSession(Command{ID: "ahp-create:" + params.Channel, Type: "create"})
	if record.err != nil {
		return nil, ahp.InvalidParams(record.err.Error())
	}
	id := record.receipt.Ref.SessionID
	c := b.session(id)
	// Until creation completes, the saved session names an incomplete one that a restart discards.
	alias = sessionAlias{Resource: params.Channel, Backend: provider, Session: id, Created: time.Now().UTC(), CreationError: "session configuration is incomplete"}
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
	p.host.mu.Lock()
	if params.ActiveClient != nil {
		a.sessionAction(types.StateAction{Value: &types.SessionActiveClientSetAction{Type: types.ActionTypeSessionActiveClientSet, ActiveClient: *params.ActiveClient}})
	}
	p.host.notifyRoot("root/sessionAdded", types.SessionAddedParams{Channel: types.RootResourceURI, Summary: a.summary()})
	p.host.mu.Unlock()
	b.ensureTaskPump(id)
	return nil, nil
}

// createChat adds a chat to a session: a fresh native session, or one forked
// from a source chat through one of its turns.
func (h *Host) createChat(raw json.RawMessage) (any, error) {
	var p types.CreateChatParams
	if json.Unmarshal(raw, &p) != nil || !ahp.ValidURI(p.Chat, "ahp-chat") {
		return nil, ahp.InvalidParams("valid chat URI required")
	}
	a, ch, err := h.ensureSession(p.Channel)
	if err != nil {
		return nil, err
	}
	if ch != nil {
		return nil, ahp.InvalidParams("session channel required")
	}
	var fork *types.ForkChatSource
	if p.Source != nil {
		var ok bool
		if fork, ok = p.Source.Value.(*types.ForkChatSource); !ok {
			return nil, ahp.InvalidParams("only forks are supported as chat sources")
		}
	}
	// Resolving attachments can open other channels. Finish it before
	// reserving this chat's URI.
	project := a.project
	var initial Command
	if p.InitialMessage != nil {
		if initial, err = project.turnCommand(commandID("initial", p.Chat), *p.InitialMessage); err != nil {
			return nil, err
		}
	}
	// A chat may not take the URI of a session's default chat.
	defer h.creation(ahp.SessionURI(p.Chat))()
	var compactions map[string]bool
	if fork != nil {
		h.mu.Lock()
		source := a.chat(fork.Chat)
		h.mu.Unlock()
		if source != nil {
			source.controller.mu.Lock()
			compactions = source.controller.compactionsLocked()
			source.controller.mu.Unlock()
		}
	}
	h.mu.Lock()
	_, aliased := h.chatAliases[p.Chat]
	_, sessionAliased := h.aliases[h.sessionURI(p.Chat)]
	if h.chats[p.Chat] != nil || aliased || sessionAliased || h.sessions[h.sessionURI(p.Chat)] != nil || strings.HasPrefix(p.Chat, "ahp-chat:/wingman/") {
		h.mu.Unlock()
		return nil, &ahp.Error{Code: int(types.ErrorCodeAlreadyExists), Message: "chat already exists"}
	}
	provider := a.state.Provider
	directories := a.state.WorkingDirectories
	command := Command{ID: "ahp-chat:" + p.Chat, Type: "create"}
	if fork != nil {
		source := a.chat(fork.Chat)
		if source == nil {
			h.mu.Unlock()
			return nil, ahp.InvalidParams("fork source must be a chat of this session")
		}
		i := slices.IndexFunc(source.chat.Turns, func(turn types.Turn) bool { return turn.Id == fork.TurnId })
		if i < 0 {
			h.mu.Unlock()
			return nil, ahp.InvalidParams("fork turn must be a completed turn of the source chat")
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
				return nil, ahp.InvalidParams("fork turn must follow a conversation turn")
			}
			command.InputID = source.chat.Turns[i].Id
		}
	}
	h.mu.Unlock()
	if p.Provider != nil && *p.Provider != provider {
		return nil, ahp.InvalidParams("chats use their session's provider")
	}
	for _, dir := range p.WorkingDirectories {
		if fork == nil && !slices.Contains(directories, dir) {
			return nil, ahp.InvalidParams("chat directories must belong to the session")
		}
	}
	if p.Title != nil {
		command.Title = *p.Title
	}
	b, err := project.backend(provider)
	if err != nil {
		return nil, err
	}
	if !b.Project.beginOperation() {
		return nil, errWorkspaceClosed
	}
	defer b.Project.operations.Done()
	record := b.createSession(command)
	if record.err != nil {
		return nil, ahp.InvalidParams(record.err.Error())
	}
	alias := sessionAlias{Resource: a.resource, Chat: p.Chat, Backend: provider, Session: record.receipt.Ref.SessionID, Created: time.Now().UTC()}
	if fork != nil {
		alias.Fork, alias.Turn = fork.Chat, fork.TurnId
	}
	c, err := project.loadController(alias)
	if err != nil {
		project.discard(b.session(alias.Session), alias)
		return nil, err
	}
	if alias.Title = command.Title; alias.Title == "" {
		alias.Title, _ = b.listing(alias.Session)
	}
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
	_ = ch.syncSettings(commandID("chat-config", p.Chat))
	if p.InitialMessage != nil {
		if err := ch.run(initial, p.InitialMessage.Model); err != nil {
			project.discard(c, alias)
			return nil, err
		}
	}
	return nil, nil
}

func (h *Host) listSessions(raw json.RawMessage) (any, error) {
	var p types.ListSessionsParams
	if json.Unmarshal(raw, &p) != nil || p.Cursor != nil {
		return nil, ahp.InvalidParams("invalid session list cursor")
	}
	root, err := h.root()
	if err != nil {
		return nil, err
	}
	// The agents are asked first, without the host's lock. Those that list
	// every folder's sessions have them listed by folder, so the others' are
	// listed by the projects that are open.
	everywhere := h.listEverywhere(root.Agents)
	var listed []listedNative
	builtinStores := map[string]bool{}
	for _, project := range h.loaded() {
		listed = append(listed, project.nativeSessions(root.Agents, everywhere, builtinStores)...)
	}
	for _, provider := range slices.Sorted(maps.Keys(everywhere)) {
		for _, info := range everywhere[provider] {
			listed = append(listed, listedNative{info.Dir, nativeKey{info.Dir, provider, info.ID}, info})
		}
	}
	// A session whose folder is gone cannot be opened, so it is not listed,
	// unless its project is open still. The folders are looked at without the
	// host's lock.
	gone := map[string]bool{}
	h.mu.Lock()
	for _, alias := range h.aliases {
		if h.projects[alias.Project] == nil {
			gone[alias.Project] = false
		}
	}
	for _, native := range listed {
		if h.projects[native.dir] == nil {
			gone[native.dir] = false
		}
	}
	h.mu.Unlock()
	findGoneFolders(gone)
	items := []types.SessionSummary{}
	seen := map[string]bool{}
	h.mu.Lock()
	saved := h.savedIndexLocked()
	for _, native := range listed {
		if gone[native.dir] {
			continue
		}
		if summary, ok := h.listedSummaryLocked(native, saved, seen); ok {
			items = append(items, summary)
		}
	}
	for _, a := range h.sessions {
		if !seen[a.resource] {
			items = append(items, a.summary())
			seen[a.resource] = true
		}
	}
	// Saved sessions no agent listed, such as those of projects that are not open, are listed as saved.
	for resource, alias := range h.aliases {
		if !seen[resource] && alias.Project != "" && alias.CreationError == "" && !gone[alias.Project] {
			items = append(items, h.savedSummaryLocked(alias))
		}
	}
	h.mu.Unlock()
	slices.SortFunc(items, func(a, b types.SessionSummary) int { return strings.Compare(b.ModifiedAt, a.ModifiedAt) })
	// Full retained catalog is returned without a cursor; pagination is optional.
	return types.ListSessionsResult{Items: items}, nil
}

// findGoneFolders marks the folders that no longer exist. One that cannot be
// told apart from a missing one, or does not answer in time, such as on a
// drive that is not connected, counts as there.
func findGoneFolders(folders map[string]bool) {
	type answer struct {
		dir  string
		gone bool
	}
	answers := make(chan answer, len(folders))
	for dir := range folders {
		go func() {
			_, err := os.Stat(dir)
			answers <- answer{dir, errors.Is(err, os.ErrNotExist)}
		}()
	}
	deadline := time.After(2 * time.Second)
	for range len(folders) {
		select {
		case a := <-answers:
			folders[a.dir] = a.gone
		case <-deadline:
			return
		}
	}
}

// savedSummaryLocked describes a saved session without loading its project.
func (h *Host) savedSummaryLocked(alias sessionAlias) types.SessionSummary {
	created := alias.Created.UTC().Format(time.RFC3339Nano)
	modified := cmp.Or(alias.Modified, alias.Created).UTC().Format(time.RFC3339Nano)
	title := cmp.Or(alias.Title, "Session")
	chat := ahp.ChatURI(alias.Resource)
	return types.SessionSummary{Resource: alias.Resource, Provider: alias.Backend, Title: title, Status: types.SessionStatusIdle | alias.flags(), CreatedAt: created, ModifiedAt: modified, Project: projectInfo(alias.Project), WorkingDirectories: []string{fileuri.FromPath(alias.Project)}, DefaultChat: &chat, Chats: h.chatSummariesLocked(alias.Resource, chat), Changes: alias.Changes}
}

// chatSummariesLocked lists a session's default chat and its saved forks.
func (h *Host) chatSummariesLocked(resource, chat string) []types.SessionChatSummary {
	chats := []types.SessionChatSummary{{Resource: chat}} // the default chat inherits the session's title
	for _, fork := range h.forksLocked(resource) {
		chats = append(chats, types.SessionChatSummary{Resource: fork.Chat, Title: fork.Title})
	}
	return chats
}

// listedNative is a session as its agent lists it in a folder.
type listedNative struct {
	dir  string
	key  nativeKey
	info code.SessionInfo
}

// nativeSessions asks this project's started agents for its sessions, except
// those that listed every folder's.
func (p *Project) nativeSessions(agents []types.AgentInfo, everywhere map[string][]code.SessionInfo, builtinStores map[string]bool) []listedNative {
	var listed []listedNative
	for _, info := range agents {
		if _, done := everywhere[info.Provider]; done {
			continue
		}
		root := p.dir
		if info.Provider == code.BuiltinAgentName {
			// Worktrees share their repository's journals, which one project lists.
			if root = cmp.Or(p.storageRoot, p.dir); builtinStores[root] {
				continue
			}
		}
		// Listing must not launch every installed external CLI. An external
		// backend becomes available here after first use/config discovery.
		b := p.startedBackend(info.Provider)
		if b == nil {
			continue
		}
		// An agent that cannot list its sessions has its saved ones listed.
		sessions, err := b.listSessions()
		if err != nil {
			continue
		}
		if b.id == code.BuiltinAgentName {
			builtinStores[root] = true
		}
		for _, session := range sessions {
			listed = append(listed, listedNative{p.dir, nativeKey{root, b.id, session.ID}, session})
		}
	}
	return listed
}

// listedSummaryLocked describes a session an agent listed in a folder: as the
// host holds it, as it was saved, or as the agent knows it.
func (h *Host) listedSummaryLocked(native listedNative, saved map[nativeKey]sessionAlias, seen map[string]bool) (types.SessionSummary, bool) {
	dir, backend, session := native.dir, native.key.backend, native.info
	resource := canonicalSessionURI(backend, session.ID, dir)
	alias, ok := saved[native.key]
	if ok {
		// Forked chats are listed with their session.
		if alias.Chat != "" {
			return types.SessionSummary{}, false
		}
		resource = alias.Resource
	}
	if seen[resource] {
		return types.SessionSummary{}, false
	}
	if a := h.sessions[resource]; a != nil {
		seen[a.resource] = true
		return a.summary(), true
	}
	if ok {
		if alias.CreationError != "" {
			return types.SessionSummary{}, false
		}
		if session.UpdatedAt.After(alias.Modified) {
			alias.Modified = session.UpdatedAt
		}
		seen[resource] = true
		return h.savedSummaryLocked(alias), true
	}
	stamp := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
	chat := ahp.ChatURI(resource)
	seen[resource] = true
	project, directory := projectInfo(dir), fileuri.FromPath(dir)
	// A folder's open project names it as its windows do.
	if p := h.projects[dir]; p != nil {
		project, directory = p.info(), p.directoryURI()
	}
	// No client has followed a session the host never saved, so it holds nothing new.
	return types.SessionSummary{Resource: resource, Provider: backend, Title: session.Title, Status: types.SessionStatusIdle | types.SessionStatusIsRead, CreatedAt: stamp, ModifiedAt: stamp, Project: project, WorkingDirectories: []string{directory}, DefaultChat: &chat, Chats: h.chatSummariesLocked(resource, chat)}, true
}

func (a *Chat) command(command Command) error {
	b := a.controller.backend
	if !b.Project.beginOperation() {
		return errWorkspaceClosed
	}
	defer b.Project.operations.Done()
	return a.controller.command(b.Project.ctx, command).err
}

// canonicalURI names a native session that no client named; the host finds it by its project.
func (p *Project) canonicalURI(backend, id string) string {
	return canonicalSessionURI(backend, id, p.dir)
}

// canonicalSessionURI names a native session of the project in dir.
func canonicalSessionURI(backend, id, dir string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return "ahp-session:/wingman/" + encode([]byte(backend)) + "/" + encode([]byte(id)) + "/" + encode([]byte(dir))
}

func parseCanonicalURI(resource string) (backend, id, dir string, err error) {
	parts := strings.Split(strings.TrimPrefix(resource, "ahp-session:/wingman/"), "/")
	if !strings.HasPrefix(resource, "ahp-session:/wingman/") || len(parts) != 2 && len(parts) != 3 {
		return "", "", "", errUnknownSession
	}
	values := make([]string, 3)
	for i, part := range parts {
		data, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(data) == 0 {
			return "", "", "", ahp.InvalidParams("invalid session URI")
		}
		values[i] = string(data)
	}
	return values[0], values[1], values[2], nil
}

// creation lets one request at a time create a session or its default chat;
// requests for other URIs, and so every other project, proceed meanwhile.
func (h *Host) creation(session string) (done func()) {
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
