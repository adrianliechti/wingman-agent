package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/go-chi/chi/v5"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Like VS Code's agent host, retained output is trimmed to 80,000 bytes once
// it exceeds 100,000; clients bound their own copies the same way.
const (
	ahpTerminalMaxContent     = 100_000
	ahpTerminalTrimmedContent = 80_000
)

type ahpTerminal struct {
	resource string
	session  *terminal.Session
	state    t.TerminalState // protected by host.mu
	size     int             // bytes of retained content, protected by host.mu
	opMu     sync.Mutex
	writeMu  sync.Mutex // orders input writes, which may block until the program reads
	filter   terminalOutputFilter
	removed  bool
}

func ahpHostTerminalClaim() t.TerminalClaim {
	return t.TerminalClaim{Value: &t.TerminalClientClaim{Kind: t.TerminalClaimKindClient, ClientId: ahpHostClient}}
}

func (h *ahpHost) prepareChannel(channel string) error {
	if strings.HasPrefix(channel, "ahp-changeset:") {
		return h.ensureChangeset(channel)
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
	_, _, err := h.ensureSession(channel)
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
		claim = ahpHostTerminalClaim()
	}
	cols, rows := session.Size()
	cwd := (&url.URL{Scheme: "file", Path: filepath.ToSlash(session.Cwd())}).String()
	a := &ahpTerminal{resource: resource, session: session, state: t.TerminalState{Title: session.Title(), Cwd: &cwd, Cols: ahpPtr(int64(cols)), Rows: ahpPtr(int64(rows)), Content: []t.TerminalContentPart{}, Claim: claim, IsPty: ahpPtr(true), SupportsCommandDetection: ahpPtr(false), Lifecycle: t.TerminalLifecycleState{Value: &t.TerminalRunningLifecycleState{Status: t.TerminalLifecycleStatusRunning}}}}
	snapshot, offset, output, cancel := session.SubscribeAt()
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
					offset += int64(len(chunk))
					h.terminalDataLocked(a, a.filter.feed(chunk))
					h.mu.Unlock()
					continue
				}
				// A stream cut off behind the output closes too; only a complete one reports the exit.
				if session.Exited() && offset == session.Written() {
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
				snapshot, offset, output, cancel = session.SubscribeAt()
				h.mu.Lock()
				a.filter = terminalOutputFilter{}
				clear := t.StateAction{Value: &t.TerminalClearedAction{Type: t.ActionTypeTerminalCleared}}
				ahp.ApplyActionToTerminal(&a.state, clear)
				a.size = 0
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
	action := t.StateAction{Value: &t.TerminalDataAction{Type: t.ActionTypeTerminalData, Data: data}}
	ahp.ApplyActionToTerminal(&a.state, action)
	h.publish(a.resource, action, nil, nil)
	if a.size += len(data); a.size > ahpTerminalMaxContent {
		// Only new subscribers see the shorter snapshot; current ones keep
		// rendering their own history, so no reset is broadcast.
		var content strings.Builder
		for _, part := range a.state.Content {
			if p, ok := part.Value.(*t.TerminalUnclassifiedPart); ok {
				content.WriteString(p.Value)
			}
		}
		tail := string(terminal.SafeTail([]byte(content.String()), ahpTerminalTrimmedContent))
		a.state.Content = []t.TerminalContentPart{{Value: &t.TerminalUnclassifiedPart{Type: "unclassified", Value: tail}}}
		a.size = len(tail)
	}
}

// AHP terminals are never unowned: a disconnected owner hands its terminals
// to the host, from which any client may claim them.
func (h *ahpHost) releaseTerminalsLocked(clientID string) {
	for c := range h.clients {
		if c.id == clientID {
			return
		}
	}
	released := false
	for _, a := range h.terminals {
		if claim, ok := a.state.Claim.Value.(*t.TerminalClientClaim); ok && claim.ClientId == clientID {
			action := t.StateAction{Value: &t.TerminalClaimedAction{Type: t.ActionTypeTerminalClaimed, Claim: ahpHostTerminalClaim()}}
			ahp.ApplyActionToTerminal(&a.state, action)
			h.publish(a.resource, action, nil, nil)
			released = true
		}
	}
	if released {
		h.publishTerminalsLocked()
	}
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
		return nil, &ahpRPCError{Code: int(t.ErrorCodeAlreadyExists), Message: "terminal already exists"}
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
	input, err := c.applyTerminalAction(a, p)
	if err != nil || input == nil {
		return err
	}
	// A write can block until the program reads; claims and disposal (which closes the PTY) must not wait for it.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.session.Write(input)
}

// applyTerminalAction validates and applies an action, returning any input to write.
func (c *ahpConnection) applyTerminalAction(a *ahpTerminal, p t.DispatchActionParams) ([]byte, error) {
	h := c.host
	a.opMu.Lock()
	defer a.opMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.removed {
		return nil, ahpInvalid("terminal disposed")
	}
	owned := c.ownsTerminalLocked(a)
	if action, ok := p.Action.Value.(*t.TerminalClaimedAction); ok {
		claim, valid := action.Claim.Value.(*t.TerminalClientClaim)
		if !valid || claim.ClientId == "" {
			return nil, ahpInvalid("client terminal claim required")
		}
		if !owned {
			old, clientOwned := a.state.Claim.Value.(*t.TerminalClientClaim)
			if !clientOwned || claim.ClientId != c.id {
				return nil, ahpInvalid("terminal belongs to another owner")
			}
			for peer := range h.clients {
				if peer.id == old.ClientId {
					return nil, ahpInvalid("terminal owner is still connected")
				}
			}
		}
	} else if !owned {
		return nil, ahpInvalid("terminal belongs to another client")
	}
	switch action := p.Action.Value.(type) {
	case *t.TerminalInputAction:
		if len(action.Data) > 1<<20 || a.session.Exited() {
			return nil, ahpInvalid("terminal input is unavailable")
		}
		return []byte(action.Data), nil
	case *t.TerminalResizedAction:
		if action.Cols < 1 || action.Rows < 1 || action.Cols > 1000 || action.Rows > 1000 {
			return nil, ahpInvalid("invalid terminal dimensions")
		}
		h.mu.Unlock()
		err := a.session.Resize(int(action.Cols), int(action.Rows))
		h.mu.Lock()
		if err != nil {
			return nil, err
		}
	case *t.TerminalTitleChangedAction:
		if strings.TrimSpace(action.Title) == "" || len(action.Title) > 1024 {
			return nil, ahpInvalid("invalid terminal title")
		}
	case *t.TerminalClaimedAction, *t.TerminalClearedAction:
	default:
		return nil, ahpInvalid("terminal action is server-only or unsupported")
	}
	ahp.ApplyActionToTerminal(&a.state, p.Action)
	switch p.Action.Value.(type) {
	case *t.TerminalTitleChangedAction, *t.TerminalClaimedAction:
		h.publishTerminalsLocked()
	case *t.TerminalClearedAction:
		a.size = 0
	}
	return nil, nil // dispatch publishes the authoritative acknowledgement exactly once.
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
				end, aborted := -1, false
			scan:
				for i := 2; i < len(p); i++ {
					switch {
					case p[i] == 7:
						end = i + 1
						break scan
					// Like xterm.js: ESC ends the string (as ST when '\' follows), CAN and SUB abort it.
					case p[i] == 0x1b && i+1 < len(p):
						end = i
						if p[i+1] == '\\' {
							end = i + 2
						}
						break scan
					case p[i] == 0x1b:
						break scan
					case p[i] == 0x18 || p[i] == 0x1a:
						end, aborted = i, true
						break scan
					}
				}
				if end < 0 {
					if len(p) > 64<<10 {
						f.pending = nil
					} // bound malformed escape sequences
					break
				}
				osc := string(p[2:end])
				if !aborted && (strings.HasPrefix(osc, "0;") || strings.HasPrefix(osc, "2;")) {
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

// handleTerminalStatus reports whether a terminal runs a command besides its
// shell, so closing it can ask for confirmation.
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	uri, err := url.PathUnescape(chi.URLParam(r, "terminal"))
	if err != nil {
		http.Error(w, "invalid terminal URI", http.StatusBadRequest)
		return
	}
	h := s.ahpHost()
	h.startTerminals()
	h.mu.Lock()
	terminal := h.terminals[uri]
	h.mu.Unlock()
	if terminal == nil {
		http.Error(w, "terminal not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]bool{"busy": terminal.session.HasForegroundProcess()})
}
