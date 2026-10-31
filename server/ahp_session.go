package server

import (
	"reflect"
	"slices"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ahpSession is an AHP session: shared state and its chats, each projected
// from its own native session. The default chat comes first.
type ahpSession struct {
	host     *ahpHost
	resource string
	created  string
	state    t.SessionState
	chats    []*ahpChat
	deleted  bool
	pending  map[string]*ahpPendingAction
	acks     map[t.ActionOrigin]ahpAck
	ackOrder []t.ActionOrigin
}

func newAHPSession(h *ahpHost, resource, provider, title string, updated time.Time) *ahpSession {
	if title == "" {
		title = "New session"
	}
	return &ahpSession{host: h, resource: resource, created: updated.UTC().Format(time.RFC3339Nano), pending: map[string]*ahpPendingAction{}, acks: map[t.ActionOrigin]ahpAck{},
		state: t.SessionState{Changesets: h.changesetCatalogue(), Provider: provider, Title: title, Status: t.SessionStatusIdle, Lifecycle: t.SessionLifecycleReady, ActiveClients: []t.SessionActiveClient{}, Chats: []t.ChatSummary{}, WorkingDirectories: []string{h.directoryURI()}},
	}
}

// addChat projects a native session as a chat. Chats after the default one
// are announced with session/chatAdded.
func (a *ahpSession) addChat(c *sessionController, resource, title string, origin *t.ChatOrigin, created time.Time) *ahpChat {
	if title == "" {
		title = a.state.Title
	}
	stamp := a.created
	if !created.IsZero() {
		stamp = created.UTC().Format(time.RFC3339Nano)
	}
	ch := newAHPChat(a, c, resource, title, stamp)
	ch.chat.Origin = origin
	c.ahp = ch
	a.chats = append(a.chats, ch)
	a.host.chats[resource] = ch
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

func (a *ahpSession) chat(resource string) *ahpChat {
	for _, ch := range a.chats {
		if ch.chat.Resource == resource {
			return ch
		}
	}
	return nil
}

func (a *ahpChat) summary() t.ChatSummary {
	return t.ChatSummary{Resource: a.chat.Resource, Title: a.chat.Title, Status: a.chat.Status, ModifiedAt: a.chat.ModifiedAt, Origin: a.chat.Origin}
}

func (a *ahpSession) summary() t.SessionSummary {
	summary := t.SessionSummary{Resource: a.resource, Provider: a.state.Provider, Title: a.state.Title, Status: a.state.Status, CreatedAt: a.created, ModifiedAt: a.created, WorkingDirectories: a.state.WorkingDirectories, DefaultChat: a.state.DefaultChat, Chats: []t.SessionChatSummary{}}
	for _, ch := range a.chats {
		status := ch.chat.Status
		summary.Chats = append(summary.Chats, t.SessionChatSummary{Resource: ch.chat.Resource, Title: ch.chat.Title, Status: &status})
		summary.ModifiedAt = max(summary.ModifiedAt, ch.chat.ModifiedAt)
	}
	return summary
}

func (a *ahpSession) sessionAction(action t.StateAction) {
	ahp.ApplyActionToSession(&a.state, action)
	a.emit(a.resource, action)
}

func (a *ahpSession) removeClient(id string) {
	for _, client := range a.state.ActiveClients {
		if client.ClientId == id {
			a.sessionAction(t.StateAction{Value: &t.SessionActiveClientRemovedAction{Type: t.ActionTypeSessionActiveClientRemoved, ClientId: id}})
			return
		}
	}
}

// syncChats publishes session-level consequences of a chat's projection.
func (a *ahpSession) syncChats(previous t.SessionSummary) {
	// Session config mirrors the default chat for clients without per-chat config.
	config := ahpConfig(a.chats[0].controller.state.Settings)
	if a.state.Config == nil {
		a.state.Config = config
	}
	if !reflect.DeepEqual(a.state.Config.Values, config.Values) {
		a.sessionAction(t.StateAction{Value: &t.SessionConfigChangedAction{Type: t.ActionTypeSessionConfigChanged, Config: config.Values, Replace: ahpPtr(true)}})
	}
	// Choices change when a provider discovers models; snapshots carry the new schema.
	a.state.Config.Schema = config.Schema
	for _, ch := range a.chats {
		next := ch.summary()
		if i := slices.IndexFunc(a.state.Chats, func(s t.ChatSummary) bool { return s.Resource == next.Resource }); i >= 0 && !reflect.DeepEqual(a.state.Chats[i], next) {
			a.sessionAction(t.StateAction{Value: &t.SessionChatUpdatedAction{Type: t.ActionTypeSessionChatUpdated, Chat: next.Resource, Changes: t.PartialChatSummary{Title: &next.Title, Status: &next.Status, ModifiedAt: &next.ModifiedAt}}})
		}
	}
	a.state.Status = a.chats[0].chat.Status
	a.syncInputNeeded()
	if next := a.summary(); !reflect.DeepEqual(previous, next) {
		a.host.notifyRoot("root/sessionSummaryChanged", t.SessionSummaryChangedParams{Channel: t.RootResourceURI, Session: a.resource, Changes: t.PartialSessionSummary{Title: &next.Title, Status: &next.Status, ModifiedAt: &next.ModifiedAt, Chats: next.Chats}})
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
		delete(a.host.chats, ch.chat.Resource)
		for client := range a.host.clients {
			delete(client.subscriptions, ch.chat.Resource)
		}
		a.sessionAction(t.StateAction{Value: &t.SessionChatRemovedAction{Type: t.ActionTypeSessionChatRemoved, Chat: ch.chat.Resource}})
		a.syncChats(previous)
		a.host.forgetChatAlias(ch.chat.Resource)
		return
	}
	a.deleted = true
	for _, other := range a.chats {
		other.removed = true
		delete(a.host.chats, other.chat.Resource)
		for client := range a.host.clients {
			delete(client.subscriptions, other.chat.Resource)
		}
	}
	for client := range a.host.clients {
		delete(client.subscriptions, a.resource)
	}
	a.host.notifyRoot("root/sessionRemoved", t.SessionRemovedParams{Channel: t.RootResourceURI, Session: a.resource})
}
