package server

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const ahpTerminalBuffer = 256 << 10

type ahpTerminal struct {
	resource string
	session  *terminal.Session
	state    t.TerminalState // protected by host.mu
	opMu     sync.Mutex
	filter   terminalOutputFilter
	removed  bool
}

func (h *ahpHost) prepareChannel(channel string) error {
	if channel == ahpWorkspaceChanges {
		return h.ensureChangeset()
	}
	if strings.HasPrefix(channel, "ahp-resource-watch:") {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.watches[channel] == nil {
			return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "watch not found"}
		}
		return nil
	}
	if strings.HasPrefix(channel, "ahp-terminal:") {
		h.startTerminals()
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.terminals[channel] == nil {
			return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "terminal not found"}
		}
		return nil
	}
	_, err := h.ensureSession(channel)
	return err
}

func (h *ahpHost) startTerminals() {
	if h.server.terminals == nil {
		return
	}
	h.terminalOnce.Do(func() {
		initial, events, cancel := h.server.terminals.Subscribe()
		h.terminalMu.Lock()
		for _, session := range initial {
			h.attachTerminal(session, "", t.TerminalClaim{})
		}
		h.terminalMu.Unlock()
		h.server.background.Go(func() {
			defer func() { cancel() }()
			for {
				select {
				case <-h.done:
					return
				case event, ok := <-events:
					if !ok {
						select {
						case <-h.done:
							return
						default:
						}
						// Catalogue overflow is recoverable from the manager's atomic snapshot.
						cancel()
						var current []*terminal.Session
						current, events, cancel = h.server.terminals.Subscribe()
						h.terminalMu.Lock()
						for _, session := range current {
							h.attachTerminal(session, "", t.TerminalClaim{})
						}
						h.mu.Lock()
						for uri, item := range h.terminals {
							if !slices.ContainsFunc(current, func(s *terminal.Session) bool { return s == item.session }) {
								item.removed = true
								delete(h.terminals, uri)
							}
						}
						h.publishTerminalsLocked()
						h.mu.Unlock()
						h.terminalMu.Unlock()
						select {
						case <-h.done:
							return
						default:
						}
						continue
					}
					h.terminalMu.Lock()
					if event.Kind == "created" {
						h.attachTerminal(event.Session, "", t.TerminalClaim{})
					}
					if event.Kind == "removed" {
						h.mu.Lock()
						for uri, terminal := range h.terminals {
							if terminal.session == event.Session {
								terminal.removed = true
								delete(h.terminals, uri)
							}
						}
						h.publishTerminalsLocked()
						h.mu.Unlock()
					}
					h.terminalMu.Unlock()
				}
			}
		})
	})
}

// Caller holds terminalMu; manager locks are never acquired under host.mu.
func (h *ahpHost) attachTerminal(session *terminal.Session, resource string, claim t.TerminalClaim) *ahpTerminal {
	h.mu.Lock()
	for _, existing := range h.terminals {
		if existing.session == session {
			h.mu.Unlock()
			return existing
		}
	}
	h.mu.Unlock()
	if resource == "" {
		resource = "ahp-terminal:/wingman/" + session.ID()
	}
	if claim.Value == nil {
		claim = t.TerminalClaim{Value: &t.TerminalClientClaim{Kind: t.TerminalClaimKindClient, ClientId: "wingman-host"}}
	}
	cols, rows := session.Size()
	cwd := (&url.URL{Scheme: "file", Path: filepath.ToSlash(session.Cwd())}).String()
	a := &ahpTerminal{resource: resource, session: session, state: t.TerminalState{Title: session.Title(), Cwd: &cwd, Cols: ahpPtr(int64(cols)), Rows: ahpPtr(int64(rows)), Content: []t.TerminalContentPart{}, Claim: claim, IsPty: ahpPtr(true), SupportsCommandDetection: ahpPtr(false), Lifecycle: t.TerminalLifecycleState{Value: &t.TerminalRunningLifecycleState{Status: t.TerminalLifecycleStatusRunning}}}}
	snapshot, output, cancel := session.Subscribe()
	h.mu.Lock()
	h.terminals[resource] = a
	h.terminalDataLocked(a, a.filter.feed(snapshot))
	h.publishTerminalsLocked()
	h.mu.Unlock()
	h.server.background.Go(func() {
		defer func() { cancel() }()
		for {
			select {
			case <-h.done:
				return
			case chunk, ok := <-output:
				h.mu.Lock()
				if a.removed {
					h.mu.Unlock()
					return
				}
				if ok {
					h.terminalDataLocked(a, a.filter.feed(chunk))
					h.mu.Unlock()
					continue
				}
				if session.Exited() {
					var code *int64
					if value, known := session.ExitCode(); known {
						code = ahpPtr(int64(value))
					}
					action := t.StateAction{Value: &t.TerminalExitedAction{Type: t.ActionTypeTerminalExited, ExitCode: code}}
					ahp.ApplyActionToTerminal(&a.state, action)
					h.publish(resource, action, nil, nil)
					h.publishTerminalsLocked()
					h.mu.Unlock()
					return
				}
				// A slow output subscriber must recover a snapshot, never silently
				// append across a gap in the VT stream.
				h.mu.Unlock()
				cancel()
				snapshot, output, cancel = session.Subscribe()
				h.mu.Lock()
				a.filter = terminalOutputFilter{}
				clear := t.StateAction{Value: &t.TerminalClearedAction{Type: t.ActionTypeTerminalCleared}}
				ahp.ApplyActionToTerminal(&a.state, clear)
				h.publish(resource, clear, nil, nil)
				h.terminalDataLocked(a, a.filter.feed(snapshot))
				h.mu.Unlock()
			}
		}
	})
	return a
}

func (h *ahpHost) terminalCatalogueLocked() []t.TerminalInfo {
	items := make([]t.TerminalInfo, 0, len(h.terminals))
	for _, a := range h.terminals {
		items = append(items, t.TerminalInfo{Resource: a.resource, Title: a.state.Title, Claim: a.state.Claim, Lifecycle: a.state.Lifecycle})
	}
	slices.SortFunc(items, func(a, b t.TerminalInfo) int { return strings.Compare(a.Resource, b.Resource) })
	return items
}

func (h *ahpHost) publishTerminalsLocked() {
	h.publish(t.RootResourceURI, t.StateAction{Value: &t.RootTerminalsChangedAction{Type: t.ActionTypeRootTerminalsChanged, Terminals: h.terminalCatalogueLocked()}}, nil, nil)
}

func (h *ahpHost) terminalDataLocked(a *ahpTerminal, data string) {
	if title := a.filter.title; title != "" {
		a.filter.title = ""
		if title != a.state.Title {
			action := t.StateAction{Value: &t.TerminalTitleChangedAction{Type: t.ActionTypeTerminalTitleChanged, Title: title}}
			ahp.ApplyActionToTerminal(&a.state, action)
			h.publish(a.resource, action, nil, nil)
			h.publishTerminalsLocked()
		}
	}
	if data == "" {
		return
	}
	content := ""
	for _, part := range a.state.Content {
		if p, ok := part.Value.(*t.TerminalUnclassifiedPart); ok {
			content += p.Value
		}
	}
	if len(content)+len(data) > ahpTerminalBuffer+(64<<10) {
		data = content + data
		data = data[len(data)-ahpTerminalBuffer:]
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
		}
		clear := t.StateAction{Value: &t.TerminalClearedAction{Type: t.ActionTypeTerminalCleared}}
		ahp.ApplyActionToTerminal(&a.state, clear)
		h.publish(a.resource, clear, nil, nil)
	}
	action := t.StateAction{Value: &t.TerminalDataAction{Type: t.ActionTypeTerminalData, Data: data}}
	ahp.ApplyActionToTerminal(&a.state, action)
	h.publish(a.resource, action, nil, nil)
}

func (c *ahpConnection) createTerminal(raw json.RawMessage) (any, error) {
	h := c.host
	var p t.CreateTerminalParams
	if json.Unmarshal(raw, &p) != nil || !validAHPSessionURI(strings.Replace(p.Channel, "ahp-terminal:", "ahp-session:", 1)) || !strings.HasPrefix(p.Channel, "ahp-terminal:") {
		return nil, ahpInvalid("valid terminal channel required")
	}
	owner, ok := p.Claim.Value.(*t.TerminalClientClaim)
	if !ok || owner.ClientId != c.id {
		return nil, ahpInvalid("initial terminal claim must belong to the creating client")
	}
	if h.server.terminals == nil {
		return nil, ahpInvalid("terminal service unavailable")
	}
	h.startTerminals()
	dir, name, shell := "", "", ""
	if p.Cwd != nil {
		var err error
		dir, err = h.resourcePath(*p.Cwd)
		if err != nil {
			return nil, err
		}
	}
	if p.Name != nil {
		name = *p.Name
	}
	if value := p.Meta["wingman.shell"]; value != nil && json.Unmarshal(value, &shell) != nil {
		return nil, ahpInvalid("invalid shell")
	}
	cols, rows := int64(terminal.DefaultCols), int64(terminal.DefaultRows)
	if p.Cols != nil {
		cols = *p.Cols
	}
	if p.Rows != nil {
		rows = *p.Rows
	}
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return nil, ahpInvalid("terminal dimensions must be between 1 and 1000")
	}
	h.terminalMu.Lock()
	defer h.terminalMu.Unlock()
	h.mu.Lock()
	exists := h.terminals[p.Channel] != nil
	h.mu.Unlock()
	if exists {
		return nil, &ahpRPCError{Code: -32003, Message: "terminal already exists"}
	}
	session, err := h.server.terminals.CreateShell(shell, dir, name, int(cols), int(rows))
	if err != nil {
		return nil, err
	}
	h.attachTerminal(session, p.Channel, p.Claim)
	return struct{}{}, nil
}

func (c *ahpConnection) ownsTerminalLocked(a *ahpTerminal) bool {
	claim, ok := a.state.Claim.Value.(*t.TerminalClientClaim)
	return ok && claim.ClientId == c.id
}

func (c *ahpConnection) disposeTerminal(resource string) (any, error) {
	h := c.host
	h.mu.Lock()
	a := h.terminals[resource]
	h.mu.Unlock()
	if a == nil {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "terminal not found"}
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	h.mu.Lock()
	if !c.ownsTerminalLocked(a) {
		h.mu.Unlock()
		return nil, ahpInvalid("claim the terminal before disposing it")
	}
	a.removed = true
	delete(h.terminals, resource)
	h.publishTerminalsLocked()
	h.mu.Unlock()
	h.server.terminals.Remove(a.session.ID())
	return struct{}{}, nil
}

func (c *ahpConnection) executeTerminalAction(p t.DispatchActionParams) error {
	h := c.host
	h.mu.Lock()
	a := h.terminals[p.Channel]
	h.mu.Unlock()
	if a == nil {
		return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "terminal not found"}
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.removed {
		return ahpInvalid("terminal disposed")
	}
	owned := c.ownsTerminalLocked(a)
	if action, ok := p.Action.Value.(*t.TerminalClaimedAction); ok {
		claim, valid := action.Claim.Value.(*t.TerminalClientClaim)
		if !valid || claim.ClientId == "" {
			return ahpInvalid("client terminal claim required")
		}
		if !owned {
			old, clientOwned := a.state.Claim.Value.(*t.TerminalClientClaim)
			if !clientOwned || claim.ClientId != c.id {
				return ahpInvalid("terminal belongs to another owner")
			}
			for peer := range h.clients {
				if peer.id == old.ClientId {
					return ahpInvalid("terminal owner is still connected")
				}
			}
		}
	} else if !owned {
		return ahpInvalid("terminal belongs to another client")
	}
	switch action := p.Action.Value.(type) {
	case *t.TerminalInputAction:
		if len(action.Data) > 1<<20 || a.session.Exited() {
			return ahpInvalid("terminal input is unavailable")
		}
		// PTY writes can block; never hold the publication lock during I/O.
		h.mu.Unlock()
		err := a.session.Write([]byte(action.Data))
		h.mu.Lock()
		return err
	case *t.TerminalResizedAction:
		if action.Cols < 1 || action.Rows < 1 || action.Cols > 1000 || action.Rows > 1000 {
			return ahpInvalid("invalid terminal dimensions")
		}
		h.mu.Unlock()
		err := a.session.Resize(int(action.Cols), int(action.Rows))
		h.mu.Lock()
		if err != nil {
			return err
		}
	case *t.TerminalTitleChangedAction:
		if strings.TrimSpace(action.Title) == "" || len(action.Title) > 1024 {
			return ahpInvalid("invalid terminal title")
		}
	case *t.TerminalClaimedAction, *t.TerminalClearedAction:
	default:
		return ahpInvalid("terminal action is server-only or unsupported")
	}
	ahp.ApplyActionToTerminal(&a.state, p.Action)
	switch p.Action.Value.(type) {
	case *t.TerminalTitleChangedAction, *t.TerminalClaimedAction:
		h.publishTerminalsLocked()
	}
	return nil // dispatch publishes the authoritative acknowledgement exactly once.
}

// Streaming UTF-8 and OSC parser. Shell integration markers are removed even
// when split across PTY reads; ordinary OSC title/color sequences are retained.
type terminalOutputFilter struct {
	pending []byte
	title   string
}

func (f *terminalOutputFilter) feed(data []byte) string {
	f.pending = append(f.pending, data...)
	var out strings.Builder
	for len(f.pending) > 0 {
		p := f.pending
		if p[0] == 0x1b {
			if len(p) == 1 {
				break
			}
			if p[1] == ']' {
				end := -1
				for i := 2; i < len(p); i++ {
					if p[i] == 7 {
						end = i + 1
						break
					}
					if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
						end = i + 2
						break
					}
				}
				if end < 0 {
					if len(p) > 64<<10 {
						f.pending = nil
					} // bound malformed escape sequences
					break
				}
				osc := string(p[2:end])
				if strings.HasPrefix(osc, "0;") || strings.HasPrefix(osc, "2;") {
					title := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(osc[2:], "\x07"), "\x1b\\"))
					runes := []rune(title)
					if len(runes) > 80 {
						runes = runes[:80]
					}
					f.title = string(runes)
				}
				if !strings.HasPrefix(osc, "133;") && !strings.HasPrefix(osc, "633;") && !strings.HasPrefix(osc, "1337;") {
					out.WriteString(strings.ToValidUTF8(string(p[:end]), "�"))
				}
				f.pending = p[end:]
				continue
			}
		}
		if !utf8.FullRune(p) {
			break
		}
		r, n := utf8.DecodeRune(p)
		out.WriteRune(r)
		f.pending = p[n:]
	}
	return out.String()
}
