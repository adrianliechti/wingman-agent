package server

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Session is an AHP session: shared state and its chats, each projected
// from its own native session. The default chat comes first.
type Session struct {
	project  *Project
	resource string
	created  string
	modified string // the session's last change before it was loaded
	state    types.SessionState
	chats    []*Chat
	pending  map[string]*pendingAction
	acks     map[types.ActionOrigin]ack
	ackOrder []types.ActionOrigin
}

func newSession(p *Project, resource, provider, title string, created, modified time.Time, own types.SessionStatus) *Session {
	if title == "" {
		title = "New session"
	}
	return &Session{project: p, resource: resource, created: created.UTC().Format(time.RFC3339Nano), modified: modified.UTC().Format(time.RFC3339Nano), pending: map[string]*pendingAction{}, acks: map[types.ActionOrigin]ack{},
		state: types.SessionState{Changesets: p.changesetCatalogue(), Provider: provider, Title: title, Status: types.SessionStatusIdle | own&^activityBits, Lifecycle: types.SessionLifecycleReady, ActiveClients: []types.SessionActiveClient{}, Chats: []types.ChatSummary{}, Project: p.info(), WorkingDirectories: []string{p.directoryURI()}},
	}
}

// addChat projects a native session as a chat. Chats after the default one
// are announced with session/chatAdded. The default chat's empty title
// inherits the session's, as in VS Code's host.
func (a *Session) addChat(c *sessionController, resource, title string, origin *types.ChatOrigin, created time.Time) *Chat {
	if title == "" && len(a.chats) > 0 {
		title = a.state.Title
	}
	stamp := a.modified
	if !created.IsZero() {
		stamp = created.UTC().Format(time.RFC3339Nano)
	}
	ch := newChat(a, c, resource, title, stamp)
	// A chat starts as read as its session: the session's clients saw its history.
	ch.chat.Status = withRead(ch.chat.Status, a.state.Status.Has(types.SessionStatusIsRead))
	ch.chat.Origin = origin
	if origin != nil {
		if fork, ok := origin.Value.(*types.ChatForkOrigin); ok {
			ch.inherit(a.chat(fork.Chat), fork.TurnId)
		}
	}
	c.chat = ch
	if agent, ok := c.backend.agent.(code.HostToolAgent); ok {
		_ = agent.SetHostTools(c.ref.SessionID, ch.offerTools)
	}
	a.chats = append(a.chats, ch)
	a.project.host.chats[resource] = ch
	if len(a.chats) == 1 {
		a.state.DefaultChat = &resource
		a.state.Config = configFromSettings(c.state.Settings)
		a.state.Chats = append(a.state.Chats, ch.summary())
	} else {
		a.sessionAction(types.StateAction{Value: &types.SessionChatAddedAction{Type: types.ActionTypeSessionChatAdded, Summary: ch.summary()}})
	}
	ch.syncProjection(c, nil)
	ch.refreshChangesLocked()
	return ch
}

// inherit gives a fork what only the host knows of the turns it copies, such
// as their timing, usage and messages as they were sent.
func (a *Chat) inherit(source *Chat, through string) {
	if source == nil {
		return
	}
	i := slices.IndexFunc(source.chat.Turns, func(turn types.Turn) bool { return turn.Id == through })
	a.chat.Turns = slices.Clone(source.chat.Turns[:i+1])
	maps.Copy(a.reasoning, source.reasoning)
	for _, turn := range a.chat.Turns {
		if ending, ok := source.endings[turn.Id]; ok {
			a.endings[turn.Id] = ending
		}
		for _, part := range turn.ResponseParts {
			if tool, ok := part.Value.(*types.ToolCallResponsePart); ok && source.clientCalls[toolCallID(tool.ToolCall)] != nil {
				a.clientCalls[toolCallID(tool.ToolCall)] = source.clientCalls[toolCallID(tool.ToolCall)]
			}
		}
		for id, prompt := range source.prompts {
			if prompt.turn == turn.Id {
				a.prompts[id] = prompt
			}
		}
	}
	a.promptSeq = source.promptSeq
	maps.Copy(a.own, source.own)
}

func (a *Session) chat(resource string) *Chat {
	for _, ch := range a.chats {
		if ch.chat.Resource == resource {
			return ch
		}
	}
	return nil
}

func (a *Chat) summary() types.ChatSummary {
	return types.ChatSummary{Resource: a.chat.Resource, Title: a.chat.Title, Status: a.chat.Status, ModifiedAt: a.chat.ModifiedAt, Changes: a.chat.Changes, Origin: a.chat.Origin}
}

func parseSessionTime(stamp string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, stamp)
	return t
}

func (a *Session) summary() types.SessionSummary {
	summary := types.SessionSummary{Resource: a.resource, Provider: a.state.Provider, Title: a.state.Title, Status: a.state.Status, Activity: a.state.Activity, CreatedAt: a.created, ModifiedAt: a.modified, Project: a.state.Project, WorkingDirectories: a.state.WorkingDirectories, DefaultChat: a.state.DefaultChat, Chats: []types.SessionChatSummary{}}
	modified := parseSessionTime(a.modified)
	for i, ch := range a.chats {
		status := ch.chat.Status
		summary.Chats = append(summary.Chats, types.SessionChatSummary{Resource: ch.chat.Resource, Title: ch.chat.Title, Status: &status, Changes: ch.chat.Changes})
		if stamp := parseSessionTime(ch.chat.ModifiedAt); stamp.After(modified) {
			modified, summary.ModifiedAt = stamp, ch.chat.ModifiedAt
		}
		// The session's footprint is its default chat's; a fork's copied history would count twice.
		if i == 0 {
			summary.Changes = ch.chat.Changes
		}
	}
	return summary
}

// configure changes the session's values and every chat that does not set its
// own, and keeps the values as its default chat runs them, which its backend
// may have normalized. Caller does not hold host.mu.
func (a *Session) configure(id string, config map[string]json.RawMessage) error {
	values, err := configValues(config)
	if err != nil {
		return err
	}
	h := a.project.host
	h.mu.Lock()
	merged := maps.Clone(a.state.Config.Values)
	maps.Copy(merged, values)
	a.state.Config.Values = merged
	chats := slices.Clone(a.chats)
	h.mu.Unlock()
	var errs []error
	for _, ch := range chats {
		errs = append(errs, ch.syncSettings(commandID(id, ch.chat.Resource)))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	main := a.chats[0]
	normalized := maps.Clone(a.state.Config.Values)
	for key := range values {
		if _, own := main.own[key]; !own && main.config.Values[key] != nil {
			normalized[key] = main.config.Values[key]
		}
	}
	a.state.Config.Values = normalized
	return errors.Join(errs...)
}

func (a *Session) sessionAction(action types.StateAction) {
	ahp.ApplyActionToSession(&a.state, action)
	a.emit(a.resource, action)
}

// keep holds in the session's alias what a host lists before it opens the
// session's project: its title, read and archived flags, last change and the
// files it changed. Caller holds host.mu.
func (a *Session) keep(summary types.SessionSummary) {
	alias, ok := a.project.host.aliases[a.resource]
	modified := parseSessionTime(summary.ModifiedAt)
	archived := a.state.Status & types.SessionStatusIsArchived
	unread := !a.state.Status.Has(types.SessionStatusIsRead)
	if !ok {
		// A native CLI session can also acquire host-owned metadata, for
		// example when a user archives it from the session list.
		main := a.chats[0]
		if !main.projected || a.resource != a.project.canonicalURI(a.state.Provider, main.controller.ref.SessionID) {
			return
		}
		created := parseSessionTime(a.created)
		alias = sessionAlias{Resource: a.resource, Project: a.project.dir, Backend: a.state.Provider, Session: main.controller.ref.SessionID, Created: created}
		if alias.Backend == code.BuiltinAgentName {
			alias.StorageRoot = a.project.storageRoot
		}
	}
	if alias.Title == a.state.Title && alias.Status == archived && alias.Unread == unread && alias.Modified.Equal(modified) && reflect.DeepEqual(alias.Changes, summary.Changes) {
		return
	}
	alias.Title, alias.Status, alias.Unread, alias.Modified, alias.Changes = a.state.Title, archived, unread, modified, summary.Changes
	a.project.host.rememberAliasLocked(alias)
	a.project.host.stageAliasLocked(alias, false)
}

func (a *Session) removeClient(id string) {
	for _, client := range a.state.ActiveClients {
		if client.ClientId == id {
			a.sessionAction(types.StateAction{Value: &types.SessionActiveClientRemovedAction{Type: types.ActionTypeSessionActiveClientRemoved, ClientId: id}})
			for _, ch := range a.chats {
				ch.clientLeft(id)
			}
			return
		}
	}
}

// activityBits are the status bits a session derives from its chats; its
// read and archived flags are its own.
const activityBits = types.SessionStatus(1<<5 - 1)

// activity is the default chat's, unless another chat needs input or failed.
func (a *Session) activity() types.SessionStatus {
	activity := a.chats[0].chat.Status & activityBits
	failed := false
	for _, ch := range a.chats[1:] {
		if ch.chat.Status.Has(types.SessionStatusInputNeeded) {
			return types.SessionStatusInputNeeded
		}
		failed = failed || ch.chat.Status.Has(types.SessionStatusError)
	}
	if failed && !activity.Has(types.SessionStatusInputNeeded) {
		return types.SessionStatusError
	}
	return activity
}

// syncChats publishes session-level consequences of a chat's projection.
func (a *Session) syncChats(previous types.SessionSummary) {
	// The session's values are its own; a setting it lacks takes the default chat's value.
	config := a.chats[0].config
	values := map[string]json.RawMessage{}
	for key, value := range config.Values {
		if own, ok := a.state.Config.Values[key]; ok {
			value = own
		}
		values[key] = value
	}
	if !reflect.DeepEqual(a.state.Config.Values, values) {
		a.sessionAction(types.StateAction{Value: &types.SessionConfigChangedAction{Type: types.ActionTypeSessionConfigChanged, Config: values, Replace: new(true)}})
		for _, ch := range a.chats {
			ch.syncOverlay()
		}
	}
	// Choices change when a provider discovers models; snapshots carry the new schema.
	a.state.Config.Schema = config.Schema
	for _, ch := range a.chats {
		next := ch.summary()
		if i := slices.IndexFunc(a.state.Chats, func(s types.ChatSummary) bool { return s.Resource == next.Resource }); i >= 0 && !reflect.DeepEqual(a.state.Chats[i], next) {
			a.sessionAction(types.StateAction{Value: &types.SessionChatUpdatedAction{Type: types.ActionTypeSessionChatUpdated, Chat: next.Resource, Changes: types.PartialChatSummary{Title: &next.Title, Status: &next.Status, ModifiedAt: &next.ModifiedAt, Changes: next.Changes}}})
		}
	}
	a.state.Status = a.activity() | a.state.Status&^activityBits
	a.syncInputNeeded()
	a.syncUnread()
	a.syncActivity()
	next := a.summary()
	if !reflect.DeepEqual(previous, next) {
		// An empty activity clears the one a client shows; a nil one would leave it.
		activity := ""
		if next.Activity != nil {
			activity = *next.Activity
		}
		a.project.host.notifyRoot("root/sessionSummaryChanged", types.SessionSummaryChangedParams{Channel: types.RootResourceURI, Session: a.resource, Changes: types.PartialSessionSummary{Title: &next.Title, Status: &next.Status, Activity: &activity, ModifiedAt: &next.ModifiedAt, Chats: next.Chats, Changes: next.Changes}})
	}
	a.keep(next)
}

func (a *Session) syncInputNeeded() {
	wanted := map[string]types.SessionInputRequest{}
	for _, ch := range a.chats {
		ch.inputRequests(wanted)
	}
	for _, request := range slices.Clone(a.state.InputNeeded) {
		id := ""
		switch r := request.Value.(type) {
		case *types.SessionChatInputRequest:
			id = r.Id
		case *types.SessionToolConfirmationRequest:
			id = r.Id
		case *types.SessionToolClientExecutionRequest:
			id = r.Id
		}
		if _, ok := wanted[id]; !ok {
			a.sessionAction(types.StateAction{Value: &types.SessionInputNeededRemovedAction{Type: types.ActionTypeSessionInputNeededRemoved, Id: id}})
		}
	}
	for _, request := range wanted {
		if !slices.ContainsFunc(a.state.InputNeeded, func(old types.SessionInputRequest) bool { return reflect.DeepEqual(old, request) }) {
			a.sessionAction(types.StateAction{Value: &types.SessionInputNeededSetAction{Type: types.ActionTypeSessionInputNeededSet, Request: request}})
		}
	}
}

func (a *Chat) inputRequests(wanted map[string]types.SessionInputRequest) {
	// A prompt raised outside a turn has no part to answer, only this request.
	for _, p := range a.prompts {
		if p.turn == "" && p.response == nil {
			wanted[p.view.ID] = types.SessionInputRequest{Value: &types.SessionChatInputRequest{Id: p.view.ID, Chat: a.chat.Resource, Kind: types.SessionInputRequestKindChatInput, Request: promptRequest(p)}}
		}
	}
	active := a.chat.ActiveTurn
	if active == nil {
		return
	}
	for _, part := range active.ResponseParts {
		switch p := part.Value.(type) {
		case *types.InputRequestResponsePart:
			if p.Response == nil {
				wanted[p.Request.Id] = types.SessionInputRequest{Value: &types.SessionChatInputRequest{Id: p.Request.Id, Chat: a.chat.Resource, Kind: types.SessionInputRequestKindChatInput, Request: p.Request}}
			}
		case *types.ToolCallResponsePart:
			if call, ok := p.ToolCall.Value.(*types.ToolCallPendingConfirmationState); ok {
				wanted[call.ToolCallId] = types.SessionInputRequest{Value: &types.SessionToolConfirmationRequest{Id: call.ToolCallId, Chat: a.chat.Resource, Kind: types.SessionInputRequestKindToolConfirmation, TurnId: active.Id, ToolCall: types.ToolCallConfirmationState{Value: call}}}
			}
			// A client finds the calls to its tools here, without following the chat.
			if call, ok := p.ToolCall.Value.(*types.ToolCallRunningState); ok && call.Contributor != nil {
				if client, ok := call.Contributor.Value.(*types.ToolCallClientContributor); ok {
					wanted[call.ToolCallId] = types.SessionInputRequest{Value: &types.SessionToolClientExecutionRequest{Id: call.ToolCallId, Chat: a.chat.Resource, Kind: types.SessionInputRequestKindToolClientExecution, TurnId: active.Id, ClientId: client.ClientId, ToolCall: *call}}
				}
			}
		}
	}
}

// chatDeleted removes a disposed chat. Disposing the default chat's native
// session disposes the whole session, which clients learn of from the root.
func (a *Session) chatDeleted(ch *Chat) {
	if ch != a.chats[0] {
		a.project.host.forgetEverywhere()
		previous := a.summary()
		a.chats = slices.DeleteFunc(a.chats, func(other *Chat) bool { return other == ch })
		delete(a.project.host.chats, ch.chat.Resource)
		for client := range a.project.host.clients {
			delete(client.subscriptions, ch.chat.Resource)
		}
		a.sessionAction(types.StateAction{Value: &types.SessionChatRemovedAction{Type: types.ActionTypeSessionChatRemoved, Chat: ch.chat.Resource}})
		a.syncChats(previous)
		a.project.host.forgetChatAliasLocked(ch.chat.Resource)
		return
	}
	for _, other := range a.chats {
		// The session's subscribers learn of every chat that goes with it.
		a.sessionAction(types.StateAction{Value: &types.SessionChatRemovedAction{Type: types.ActionTypeSessionChatRemoved, Chat: other.chat.Resource}})
		delete(a.project.host.chats, other.chat.Resource)
		for client := range a.project.host.clients {
			delete(client.subscriptions, other.chat.Resource)
		}
	}
	for client := range a.project.host.clients {
		delete(client.subscriptions, a.resource)
	}
	delete(a.project.host.sessions, a.resource)
	// A chat created while the session was disposed goes with it.
	a.project.host.forgetSessionLocked(a.resource)
}
