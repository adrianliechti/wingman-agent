package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
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
	project  *ahpProject
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

// Terminal URI schemes are server-defined. Accept client-chosen schemes
// (including VS Code's agenthost-terminal), but reserve other ahp-* namespaces
// for their protocol channels. Never rewrite the terminal's subscription identity.
func validAHPTerminalURI(resource string) bool {
	u, err := url.Parse(resource)
	if err != nil || u.Scheme == "" || strings.HasPrefix(u.Scheme, "ahp-") && u.Scheme != "ahp-terminal" {
		return false
	}
	return validAHPURI(resource, u.Scheme)
}

func (h *ahpHost) terminal(resource string) *ahpTerminal {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.ownerLocked(resource); p != nil {
		return p.terminals[resource]
	}
	return nil
}

// snapshotLocked fills in the state of one of the project's own channels.
func (p *ahpProject) snapshotLocked(channel string, state *t.SnapshotState) bool {
	if terminal := p.terminals[channel]; terminal != nil {
		state.Terminal = &terminal.state
	} else if watch := p.watches[channel]; watch != nil {
		state.ResourceWatch = &watch.state
		watch.touched = time.Now()
	} else if changeset := p.changesets[channel]; changeset != nil {
		state.Changeset = &changeset.state
	} else {
		return false
	}
	return true
}

func (h *ahpHost) prepareChannel(channel string) error {
	for _, p := range h.loaded() {
		p.startTerminals()
	}
	if p := h.owner(channel); p != nil {
		if strings.HasPrefix(channel, "ahp-changeset:") {
			return p.ensureChangeset(channel)
		}
		return nil
	}
	switch {
	case strings.HasPrefix(channel, "ahp-changeset:"):
		return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "changeset is no longer available; compare revisions again"}
	case strings.HasPrefix(channel, "ahp-resource-watch:"):
		return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "watch not found"}
	case validAHPTerminalURI(channel):
		return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "terminal not found"}
	}
	_, _, err := h.ensureSession(channel)
	return err
}

func (p *ahpProject) startTerminals() {
	if p.server.terminals == nil {
		return
	}
	p.terminalOnce.Do(func() {
		initial, events, cancel := p.server.terminals.Subscribe()
		p.terminalMu.Lock()
		for _, session := range initial {
			p.attachTerminal(session, "", t.TerminalClaim{})
		}
		p.terminalMu.Unlock()
		p.server.background.Go(func() {
			defer func() { cancel() }()
			for {
				select {
				case <-p.server.ctx.Done():
					return
				case event, ok := <-events:
					if !ok {
						select {
						case <-p.server.ctx.Done():
							return
						default:
						}
						// Catalogue overflow is recoverable from the manager's atomic snapshot.
						cancel()
						var current []*terminal.Session
						current, events, cancel = p.server.terminals.Subscribe()
						p.terminalMu.Lock()
						for _, session := range current {
							p.attachTerminal(session, "", t.TerminalClaim{})
						}
						p.mu.Lock()
						for uri, item := range p.terminals {
							if !slices.ContainsFunc(current, func(s *terminal.Session) bool { return s == item.session }) {
								item.removed = true
								delete(p.terminals, uri)
							}
						}
						p.publishTerminalsLocked()
						p.mu.Unlock()
						p.terminalMu.Unlock()
						select {
						case <-p.server.ctx.Done():
							return
						default:
						}
						continue
					}
					p.terminalMu.Lock()
					if event.Kind == "created" {
						p.attachTerminal(event.Session, "", t.TerminalClaim{})
					}
					if event.Kind == "removed" {
						p.mu.Lock()
						for uri, terminal := range p.terminals {
							if terminal.session == event.Session {
								terminal.removed = true
								delete(p.terminals, uri)
							}
						}
						p.publishTerminalsLocked()
						p.mu.Unlock()
					}
					p.terminalMu.Unlock()
				}
			}
		})
	})
}

// Caller holds terminalMu; manager locks are never acquired under host.mu.
func (p *ahpProject) attachTerminal(session *terminal.Session, resource string, claim t.TerminalClaim) *ahpTerminal {
	p.mu.Lock()
	for _, existing := range p.terminals {
		if existing.session == session {
			p.mu.Unlock()
			return existing
		}
	}
	p.mu.Unlock()
	if resource == "" {
		resource = "ahp-terminal:/" + p.namespace + "/" + session.ID()
	}
	if claim.Value == nil {
		claim = ahpHostTerminalClaim()
	}
	cols, rows := session.Size()
	cwd := fileuri.FromPath(session.Cwd())
	a := &ahpTerminal{project: p, resource: resource, session: session, state: t.TerminalState{Title: session.Title(), Cwd: &cwd, Cols: new(int64(cols)), Rows: new(int64(rows)), Content: []t.TerminalContentPart{}, Claim: claim, IsPty: new(true), SupportsCommandDetection: new(false), Lifecycle: t.TerminalLifecycleState{Value: &t.TerminalRunningLifecycleState{Status: t.TerminalLifecycleStatusRunning}}}}
	snapshot, offset, output, cancel := session.SubscribeAt()
	p.mu.Lock()
	p.terminals[resource] = a
	p.terminalDataLocked(a, a.filter.feed(snapshot))
	p.publishTerminalsLocked()
	p.mu.Unlock()
	p.server.background.Go(func() {
		defer func() { cancel() }()
		for {
			select {
			case <-p.server.ctx.Done():
				return
			case chunk, ok := <-output:
				p.mu.Lock()
				if a.removed {
					p.mu.Unlock()
					return
				}
				if ok {
					offset += int64(len(chunk))
					p.terminalDataLocked(a, a.filter.feed(chunk))
					p.mu.Unlock()
					continue
				}
				// A stream cut off behind the output closes too; only a complete one reports the exit.
				if session.Exited() && offset == session.Written() {
					var code *int64
					if value, known := session.ExitCode(); known {
						code = new(int64(value))
					}
					action := t.StateAction{Value: &t.TerminalExitedAction{Type: t.ActionTypeTerminalExited, ExitCode: code}}
					ahp.ApplyActionToTerminal(&a.state, action)
					p.publish(resource, action, nil, nil)
					p.publishTerminalsLocked()
					p.mu.Unlock()
					return
				}
				// A slow output subscriber must recover a snapshot, never silently
				// append across a gap in the VT stream.
				p.mu.Unlock()
				cancel()
				snapshot, offset, output, cancel = session.SubscribeAt()
				p.mu.Lock()
				a.filter = terminalOutputFilter{}
				clear := t.StateAction{Value: &t.TerminalClearedAction{Type: t.ActionTypeTerminalCleared}}
				ahp.ApplyActionToTerminal(&a.state, clear)
				a.size = 0
				p.publish(resource, clear, nil, nil)
				p.terminalDataLocked(a, a.filter.feed(snapshot))
				p.mu.Unlock()
			}
		}
	})
	return a
}

// terminalsLocked lists the terminals of every project.
func (h *ahpHost) terminalsLocked() []t.TerminalInfo {
	items := []t.TerminalInfo{}
	for _, p := range h.projectsLocked() {
		for _, a := range p.terminals {
			items = append(items, t.TerminalInfo{Resource: a.resource, Title: a.state.Title, Claim: a.state.Claim, Lifecycle: a.state.Lifecycle})
		}
	}
	slices.SortFunc(items, func(a, b t.TerminalInfo) int { return strings.Compare(a.Resource, b.Resource) })
	return items
}

func (h *ahpHost) publishTerminalsLocked() {
	h.publish(t.RootResourceURI, t.StateAction{Value: &t.RootTerminalsChangedAction{Type: t.ActionTypeRootTerminalsChanged, Terminals: h.terminalsLocked()}}, nil, nil)
}

func (p *ahpProject) terminalDataLocked(a *ahpTerminal, data string) {
	if title := a.filter.title; title != "" {
		a.filter.title = ""
		if title != a.state.Title {
			action := t.StateAction{Value: &t.TerminalTitleChangedAction{Type: t.ActionTypeTerminalTitleChanged, Title: title}}
			ahp.ApplyActionToTerminal(&a.state, action)
			p.publish(a.resource, action, nil, nil)
			p.publishTerminalsLocked()
		}
	}
	if data == "" {
		return
	}
	action := t.StateAction{Value: &t.TerminalDataAction{Type: t.ActionTypeTerminalData, Data: data}}
	ahp.ApplyActionToTerminal(&a.state, action)
	p.publish(a.resource, action, nil, nil)
	if a.size += len(data); a.size > ahpTerminalMaxContent {
		// Only new subscribers see the shorter snapshot; current ones keep
		// rendering their own history, so no reset is broadcast.
		var content strings.Builder
		for _, part := range a.state.Content {
			if text, ok := part.Value.(*t.TerminalUnclassifiedPart); ok {
				content.WriteString(text.Value)
			}
		}
		tail := string(terminal.SafeTail([]byte(content.String()), ahpTerminalTrimmedContent))
		a.state.Content = []t.TerminalContentPart{{Value: &t.TerminalUnclassifiedPart{Type: "unclassified", Value: tail}}}
		a.size = len(tail)
	}
}

// AHP terminals are never unowned: a disconnected owner hands its terminals
// to the host, from which any client may claim them.
func (p *ahpProject) releaseTerminalsLocked(clientID string) {
	for c := range p.clients {
		if c.id == clientID {
			return
		}
	}
	released := false
	for _, a := range p.terminals {
		if claim, ok := a.state.Claim.Value.(*t.TerminalClientClaim); ok && claim.ClientId == clientID {
			action := t.StateAction{Value: &t.TerminalClaimedAction{Type: t.ActionTypeTerminalClaimed, Claim: ahpHostTerminalClaim()}}
			ahp.ApplyActionToTerminal(&a.state, action)
			p.publish(a.resource, action, nil, nil)
			released = true
		}
	}
	if released {
		p.publishTerminalsLocked()
	}
}

func (c *ahpConnection) createTerminal(raw json.RawMessage) (any, error) {
	var p t.CreateTerminalParams
	if json.Unmarshal(raw, &p) != nil || !validAHPTerminalURI(p.Channel) {
		return nil, ahpInvalid("valid terminal channel required")
	}
	project, err := c.host.loadedProject(p.Cwd)
	if err != nil {
		return nil, err
	}
	owner, ok := p.Claim.Value.(*t.TerminalClientClaim)
	if !ok || owner.ClientId != c.id {
		return nil, ahpInvalid("initial terminal claim must belong to the creating client")
	}
	if project.server.terminals == nil {
		return nil, ahpInvalid("terminal service unavailable")
	}
	project.startTerminals()
	dir, name, shell := "", "", ""
	if p.Cwd != nil {
		var err error
		dir, err = project.resourcePath(*p.Cwd)
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
	project.terminalMu.Lock()
	defer project.terminalMu.Unlock()
	if project.terminal(p.Channel) != nil {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeAlreadyExists), Message: "terminal already exists"}
	}
	session, err := project.server.terminals.CreateShell(shell, dir, name, int(cols), int(rows))
	if err != nil {
		return nil, err
	}
	project.attachTerminal(session, p.Channel, p.Claim)
	return nil, nil
}

func (c *ahpConnection) ownsTerminalLocked(a *ahpTerminal) bool {
	claim, ok := a.state.Claim.Value.(*t.TerminalClientClaim)
	return ok && claim.ClientId == c.id
}

func (c *ahpConnection) disposeTerminal(resource string) (any, error) {
	a := c.host.terminal(resource)
	if a == nil {
		return nil, &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "terminal not found"}
	}
	h := c.host
	a.opMu.Lock()
	defer a.opMu.Unlock()
	h.mu.Lock()
	if !c.ownsTerminalLocked(a) {
		h.mu.Unlock()
		return nil, ahpInvalid("claim the terminal before disposing it")
	}
	a.removed = true
	delete(a.project.terminals, resource)
	h.publishTerminalsLocked()
	h.mu.Unlock()
	a.project.server.terminals.Remove(a.session.ID())
	return nil, nil
}

func (c *ahpConnection) executeTerminalAction(p t.DispatchActionParams) error {
	a := c.host.terminal(p.Channel)
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
	h := s.ahp
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
