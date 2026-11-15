package server

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpSession is an AHP session: shared state and its chats, each projected
// from its own native session. The default chat comes first.
type ahpSession struct {
	project  *ahpProject
	resource string
	created  string
	state    t.SessionState
	chats    []*ahpChat
	deleted  bool
	unsaved  bool // the alias has a title that is not saved yet
	pending  map[string]*ahpPendingAction
	acks     map[t.ActionOrigin]ahpAck
	ackOrder []t.ActionOrigin
}

func newAHPSession(h *ahpProject, resource, provider, title string, updated time.Time) *ahpSession {
	if title == "" {
		title = "New session"
	}
	return &ahpSession{project: h, resource: resource, created: updated.UTC().Format(time.RFC3339Nano), pending: map[string]*ahpPendingAction{}, acks: map[t.ActionOrigin]ahpAck{},
		state: t.SessionState{Changesets: h.changesetCatalogue(), Provider: provider, Title: title, Status: t.SessionStatusIdle, Lifecycle: t.SessionLifecycleReady, ActiveClients: []t.SessionActiveClient{}, Chats: []t.ChatSummary{}, Project: h.info(), WorkingDirectories: []string{h.directoryURI()}},
	}
}

// addChat projects a native session as a chat. Chats after the default one
// are announced with session/chatAdded. The default chat's empty title
// inherits the session's, as in VS Code's host.
func (a *ahpSession) addChat(c *sessionController, resource, title string, origin *t.ChatOrigin, created time.Time) *ahpChat {
	if title == "" && len(a.chats) > 0 {
		title = a.state.Title
	}
	stamp := a.created
	if !created.IsZero() {
		stamp = created.UTC().Format(time.RFC3339Nano)
	}
	ch := newAHPChat(a, c, resource, title, stamp)
	ch.chat.Origin = origin
	if origin != nil {
		if fork, ok := origin.Value.(*t.ChatForkOrigin); ok {
			ch.inherit(a.chat(fork.Chat), fork.TurnId)
		}
	}
	c.ahp = ch
	if agent, ok := c.backend.agent.(code.HostToolAgent); ok {
		_ = agent.SetHostTools(c.ref.SessionID, ch.offerTools)
	}
	a.chats = append(a.chats, ch)
	a.project.chats[resource] = ch
	if len(a.chats) == 1 {
		a.state.DefaultChat = &resource
		a.state.Config = ahpConfig(c.state.Settings)
		a.state.Chats = append(a.state.Chats, ch.summary())
	} else {
		a.sessionAction(t.StateAction{Value: &t.SessionChatAddedAction{Type: t.ActionTypeSessionChatAdded, Summary: ch.summary()}})
	}
	ch.syncProjection(c, nil)
	return ch
}

// inherit gives a fork what only the host knows of the turns it copies, such
// as their timing, usage and messages as they were sent.
func (a *ahpChat) inherit(source *ahpChat, through string) {
	if source == nil {
		return
	}
	i := slices.IndexFunc(source.chat.Turns, func(turn t.Turn) bool { return turn.Id == through })
	a.chat.Turns = slices.Clone(source.chat.Turns[:i+1])
	maps.Copy(a.reasoning, source.reasoning)
	for _, turn := range a.chat.Turns {
		for _, part := range turn.ResponseParts {
			if tool, ok := part.Value.(*t.ToolCallResponsePart); ok && source.clientCalls[ahpToolCallID(tool.ToolCall)] != nil {
				a.clientCalls[ahpToolCallID(tool.ToolCall)] = source.clientCalls[ahpToolCallID(tool.ToolCall)]
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

func (a *ahpSession) chat(resource string) *ahpChat {
	for _, ch := range a.chats {
		if ch.chat.Resource == resource {
			return ch
		}
	}
	return nil
}

func (a *ahpChat) summary() t.ChatSummary {
	return t.ChatSummary{Resource: a.chat.Resource, Title: a.title, Status: a.chat.Status, ModifiedAt: a.chat.ModifiedAt, Origin: a.chat.Origin}
}

func (a *ahpSession) summary() t.SessionSummary {
	summary := t.SessionSummary{Resource: a.resource, Provider: a.state.Provider, Title: a.state.Title, Status: a.state.Status, CreatedAt: a.created, ModifiedAt: a.created, Project: a.state.Project, WorkingDirectories: a.state.WorkingDirectories, DefaultChat: a.state.DefaultChat, Chats: []t.SessionChatSummary{}}
	for _, ch := range a.chats {
		status := ch.chat.Status
		summary.Chats = append(summary.Chats, t.SessionChatSummary{Resource: ch.chat.Resource, Title: ch.title, Status: &status})
		summary.ModifiedAt = max(summary.ModifiedAt, ch.chat.ModifiedAt)
	}
	return summary
}

func (a *ahpSession) sessionAction(action t.StateAction) {
	ahp.ApplyActionToSession(&a.state, action)
	a.emit(a.resource, action)
}

// A host lists the saved title before it loads the session's project. Caller holds host.mu.
func (a *ahpSession) rememberTitle() {
	alias, ok := a.project.aliases[a.resource]
	if !ok || alias.Title == a.state.Title {
		return
	}
	alias.Title = a.state.Title
	a.project.rememberAliasLocked(alias)
	a.unsaved = true
}

// saveTitle writes a remembered title outside the host's lock, which the actions of every project wait for.
func (a *ahpSession) saveTitle() {
	p := a.project
	p.aliasMu.Lock()
	defer p.aliasMu.Unlock()
	p.mu.Lock()
	alias, saved := p.aliases[a.resource]
	unsaved := a.unsaved
	a.unsaved = false
	p.mu.Unlock()
	if !saved || !unsaved {
		return
	}
	_ = p.saveAlias(alias)
	// Disposing the session meanwhile removed its alias.
	p.mu.Lock()
	_, saved = p.aliases[a.resource]
	p.mu.Unlock()
	if !saved {
		p.removeAlias(alias)
	}
}

func (a *ahpSession) removeClient(id string) {
	for _, client := range a.state.ActiveClients {
		if client.ClientId == id {
			a.sessionAction(t.StateAction{Value: &t.SessionActiveClientRemovedAction{Type: t.ActionTypeSessionActiveClientRemoved, ClientId: id}})
			for _, ch := range a.chats {
				ch.clientLeft(id)
			}
			return
		}
	}
}

// ahpActivityBits are the status bits a session derives from its chats; its
// read and archived flags are its own.
const ahpActivityBits = t.SessionStatus(1<<5 - 1)

// activity is the default chat's, unless another chat needs input or failed.
func (a *ahpSession) activity() t.SessionStatus {
	activity := a.chats[0].chat.Status & ahpActivityBits
	failed := false
	for _, ch := range a.chats[1:] {
		if ch.chat.Status.Has(t.SessionStatusInputNeeded) {
			return t.SessionStatusInputNeeded
		}
		failed = failed || ch.chat.Status.Has(t.SessionStatusError)
	}
	if failed && !activity.Has(t.SessionStatusInputNeeded) {
		return t.SessionStatusError
	}
	return activity
}

// syncChats publishes session-level consequences of a chat's projection.
func (a *ahpSession) syncChats(previous t.SessionSummary) {
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
		a.sessionAction(t.StateAction{Value: &t.SessionConfigChangedAction{Type: t.ActionTypeSessionConfigChanged, Config: values, Replace: new(true)}})
		for _, ch := range a.chats {
			ch.syncOverlay()
		}
	}
	// Choices change when a provider discovers models; snapshots carry the new schema.
	a.state.Config.Schema = config.Schema
	for _, ch := range a.chats {
		next := ch.summary()
		if i := slices.IndexFunc(a.state.Chats, func(s t.ChatSummary) bool { return s.Resource == next.Resource }); i >= 0 && !reflect.DeepEqual(a.state.Chats[i], next) {
			a.sessionAction(t.StateAction{Value: &t.SessionChatUpdatedAction{Type: t.ActionTypeSessionChatUpdated, Chat: next.Resource, Changes: t.PartialChatSummary{Title: &next.Title, Status: &next.Status, ModifiedAt: &next.ModifiedAt}}})
		}
	}
	a.state.Status = a.activity() | a.state.Status&^ahpActivityBits
	a.syncInputNeeded()
	if next := a.summary(); !reflect.DeepEqual(previous, next) {
		a.project.notifyRoot("root/sessionSummaryChanged", t.SessionSummaryChangedParams{Channel: t.RootResourceURI, Session: a.resource, Changes: t.PartialSessionSummary{Title: &next.Title, Status: &next.Status, ModifiedAt: &next.ModifiedAt, Chats: next.Chats}})
	}
}

func (a *ahpSession) syncInputNeeded() {
	wanted := map[string]t.SessionInputRequest{}
	for _, ch := range a.chats {
		ch.inputRequests(wanted)
	}
	for _, request := range slices.Clone(a.state.InputNeeded) {
		id := ""
		switch r := request.Value.(type) {
		case *t.SessionChatInputRequest:
			id = r.Id
		case *t.SessionToolConfirmationRequest:
			id = r.Id
		case *t.SessionToolClientExecutionRequest:
			id = r.Id
		}
		if _, ok := wanted[id]; !ok {
			a.sessionAction(t.StateAction{Value: &t.SessionInputNeededRemovedAction{Type: t.ActionTypeSessionInputNeededRemoved, Id: id}})
		}
	}
	for _, request := range wanted {
		if !slices.ContainsFunc(a.state.InputNeeded, func(old t.SessionInputRequest) bool { return reflect.DeepEqual(old, request) }) {
			a.sessionAction(t.StateAction{Value: &t.SessionInputNeededSetAction{Type: t.ActionTypeSessionInputNeededSet, Request: request}})
		}
	}
}

func (a *ahpChat) inputRequests(wanted map[string]t.SessionInputRequest) {
	// A prompt raised outside a turn has no part to answer, only this request.
	for _, p := range a.prompts {
		if p.turn == "" && p.response == nil {
			wanted[p.view.ID] = t.SessionInputRequest{Value: &t.SessionChatInputRequest{Id: p.view.ID, Chat: a.chat.Resource, Kind: t.SessionInputRequestKindChatInput, Request: ahpPromptRequest(p)}}
		}
	}
	active := a.chat.ActiveTurn
	if active == nil {
		return
	}
	for _, part := range active.ResponseParts {
		switch p := part.Value.(type) {
		case *t.InputRequestResponsePart:
			if p.Response == nil {
				wanted[p.Request.Id] = t.SessionInputRequest{Value: &t.SessionChatInputRequest{Id: p.Request.Id, Chat: a.chat.Resource, Kind: t.SessionInputRequestKindChatInput, Request: p.Request}}
			}
		case *t.ToolCallResponsePart:
			if call, ok := p.ToolCall.Value.(*t.ToolCallPendingConfirmationState); ok {
				wanted[call.ToolCallId] = t.SessionInputRequest{Value: &t.SessionToolConfirmationRequest{Id: call.ToolCallId, Chat: a.chat.Resource, Kind: t.SessionInputRequestKindToolConfirmation, TurnId: active.Id, ToolCall: t.ToolCallConfirmationState{Value: call}}}
			}
			// A client finds the calls to its tools here, without following the chat.
			if call, ok := p.ToolCall.Value.(*t.ToolCallRunningState); ok && call.Contributor != nil {
				if client, ok := call.Contributor.Value.(*t.ToolCallClientContributor); ok {
					wanted[call.ToolCallId] = t.SessionInputRequest{Value: &t.SessionToolClientExecutionRequest{Id: call.ToolCallId, Chat: a.chat.Resource, Kind: t.SessionInputRequestKindToolClientExecution, TurnId: active.Id, ClientId: client.ClientId, ToolCall: *call}}
				}
			}
		}
	}
}

// chatDeleted removes a disposed chat. Disposing the default chat's native
// session disposes the whole session, which clients learn of from the root.
func (a *ahpSession) chatDeleted(ch *ahpChat) {
	if ch != a.chats[0] {
		previous := a.summary()
		ch.removed = true
		a.chats = slices.DeleteFunc(a.chats, func(other *ahpChat) bool { return other == ch })
		delete(a.project.chats, ch.chat.Resource)
		for client := range a.project.clients {
			delete(client.subscriptions, ch.chat.Resource)
		}
		a.sessionAction(t.StateAction{Value: &t.SessionChatRemovedAction{Type: t.ActionTypeSessionChatRemoved, Chat: ch.chat.Resource}})
		a.syncChats(previous)
		a.project.forgetChatAlias(ch.chat.Resource)
		return
	}
	a.deleted = true
	for _, other := range a.chats {
		other.removed = true
		// The session's subscribers learn of every chat that goes with it.
		a.sessionAction(t.StateAction{Value: &t.SessionChatRemovedAction{Type: t.ActionTypeSessionChatRemoved, Chat: other.chat.Resource}})
		delete(a.project.chats, other.chat.Resource)
		for client := range a.project.clients {
			delete(client.subscriptions, other.chat.Resource)
		}
	}
	for client := range a.project.clients {
		delete(client.subscriptions, a.resource)
	}
	a.project.forgetAlias(a.resource)
	a.project.notifyRoot("root/sessionRemoved", t.SessionRemovedParams{Channel: t.RootResourceURI, Session: a.resource})
}
