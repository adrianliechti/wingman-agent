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
	"github.com/adrianliechti/wingman-agent/server/ahp"
	"github.com/go-chi/chi/v5"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Like VS Code's agent host, retained output is trimmed to 80,000 bytes once
// it exceeds 100,000; clients bound their own copies the same way.
const (
	terminalMaxContent     = 100_000
	terminalTrimmedContent = 80_000
)

type terminalChannel struct {
	project  *Project
	resource string
	session  *terminal.Session
	state    types.TerminalState // protected by host.mu
	size     int                 // bytes of retained content, protected by host.mu
	opMu     sync.Mutex          // orders claims, disposal and resize while PTY I/O releases host.mu
	writeMu  sync.Mutex          // orders input writes, which may block until the program reads
	filter   terminalOutputFilter
}

func hostTerminalClaim() types.TerminalClaim {
	return types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: hostClient}}
}

func (h *Host) terminal(resource string) *terminalChannel {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.ownerLocked(resource); p != nil {
		return p.terminalChannels[resource]
	}
	return nil
}

// snapshotLocked fills in the state of one of the project's own channels.
func (p *Project) snapshotLocked(channel string, state *types.SnapshotState) bool {
	if terminal := p.terminalChannels[channel]; terminal != nil {
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

func (h *Host) prepareChannel(channel string) error {
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
		return &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "changeset is no longer available; compare revisions again"}
	case strings.HasPrefix(channel, "ahp-resource-watch:"):
		return &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "watch not found"}
	case ahp.ValidTerminalURI(channel):
		return &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "terminal not found"}
	}
	_, _, err := h.ensureSession(channel)
	return err
}

func (p *Project) startTerminals() {
	if p.terminals == nil {
		return
	}
	p.terminalOnce.Do(func() {
		initial, events, cancel := p.terminals.Subscribe()
		p.host.terminalMu.Lock()
		for _, session := range initial {
			p.attachTerminal(session, "", types.TerminalClaim{})
		}
		p.host.terminalMu.Unlock()
		p.background.Go(func() {
			defer func() { cancel() }()
			for {
				select {
				case <-p.ctx.Done():
					return
				case event, ok := <-events:
					if !ok {
						select {
						case <-p.ctx.Done():
							return
						default:
						}
						// Catalogue overflow is recoverable from the manager's atomic snapshot.
						cancel()
						var current []*terminal.Session
						current, events, cancel = p.terminals.Subscribe()
						p.host.terminalMu.Lock()
						for _, session := range current {
							p.attachTerminal(session, "", types.TerminalClaim{})
						}
						p.host.mu.Lock()
						for uri, item := range p.terminalChannels {
							if !slices.ContainsFunc(current, func(s *terminal.Session) bool { return s == item.session }) {
								delete(p.terminalChannels, uri)
							}
						}
						p.host.publishTerminalsLocked()
						p.host.mu.Unlock()
						p.host.terminalMu.Unlock()
						select {
						case <-p.ctx.Done():
							return
						default:
						}
						continue
					}
					p.host.terminalMu.Lock()
					if event.Kind == "created" {
						p.attachTerminal(event.Session, "", types.TerminalClaim{})
					}
					if event.Kind == "removed" {
						p.host.mu.Lock()
						for uri, terminal := range p.terminalChannels {
							if terminal.session == event.Session {
								delete(p.terminalChannels, uri)
							}
						}
						p.host.publishTerminalsLocked()
						p.host.mu.Unlock()
					}
					p.host.terminalMu.Unlock()
				}
			}
		})
	})
}

// Caller holds terminalMu; manager locks are never acquired under host.mu.
func (p *Project) attachTerminal(session *terminal.Session, resource string, claim types.TerminalClaim) *terminalChannel {
	p.host.mu.Lock()
	for _, existing := range p.terminalChannels {
		if existing.session == session {
			p.host.mu.Unlock()
			return existing
		}
	}
	p.host.mu.Unlock()
	if resource == "" {
		resource = "ahp-terminal:/" + p.namespace + "/" + session.ID()
	}
	if claim.Value == nil {
		claim = hostTerminalClaim()
	}
	cols, rows := session.Size()
	cwd := fileuri.FromPath(session.Cwd())
	a := &terminalChannel{project: p, resource: resource, session: session, state: types.TerminalState{Title: session.Title(), Cwd: &cwd, Cols: new(int64(cols)), Rows: new(int64(rows)), Content: []types.TerminalContentPart{}, Claim: claim, IsPty: new(true), SupportsCommandDetection: new(false), Lifecycle: types.TerminalLifecycleState{Value: &types.TerminalRunningLifecycleState{Status: types.TerminalLifecycleStatusRunning}}}}
	snapshot, offset, output, cancel := session.SubscribeAt()
	p.host.mu.Lock()
	p.terminalChannels[resource] = a
	p.terminalDataLocked(a, a.filter.feed(snapshot))
	p.host.publishTerminalsLocked()
	p.host.mu.Unlock()
	p.background.Go(func() {
		defer func() { cancel() }()
		for {
			select {
			case <-p.ctx.Done():
				return
			case chunk, ok := <-output:
				p.host.mu.Lock()
				if p.terminalChannels[resource] != a {
					p.host.mu.Unlock()
					return
				}
				if ok {
					offset += int64(len(chunk))
					p.terminalDataLocked(a, a.filter.feed(chunk))
					p.host.mu.Unlock()
					continue
				}
				// A stream cut off behind the output closes too; only a complete one reports the exit.
				if session.Exited() && offset == session.Written() {
					var code *int64
					if value, known := session.ExitCode(); known {
						code = new(int64(value))
					}
					action := types.StateAction{Value: &types.TerminalExitedAction{Type: types.ActionTypeTerminalExited, ExitCode: code}}
					sdk.ApplyActionToTerminal(&a.state, action)
					p.host.publish(resource, action, nil, nil)
					p.host.publishTerminalsLocked()
					p.host.mu.Unlock()
					return
				}
				// A slow output subscriber must recover a snapshot, never silently
				// append across a gap in the VT stream.
				p.host.mu.Unlock()
				cancel()
				snapshot, offset, output, cancel = session.SubscribeAt()
				p.host.mu.Lock()
				if p.terminalChannels[resource] != a {
					p.host.mu.Unlock()
					return
				}
				a.filter = terminalOutputFilter{}
				clear := types.StateAction{Value: &types.TerminalClearedAction{Type: types.ActionTypeTerminalCleared}}
				sdk.ApplyActionToTerminal(&a.state, clear)
				a.size = 0
				p.host.publish(resource, clear, nil, nil)
				p.terminalDataLocked(a, a.filter.feed(snapshot))
				p.host.mu.Unlock()
			}
		}
	})
	return a
}

// terminalsLocked lists the terminals of every project.
func (h *Host) terminalsLocked() []types.TerminalInfo {
	items := []types.TerminalInfo{}
	for _, p := range h.projectsLocked() {
		for _, a := range p.terminalChannels {
			items = append(items, types.TerminalInfo{Resource: a.resource, Title: a.state.Title, Claim: a.state.Claim, Lifecycle: a.state.Lifecycle})
		}
	}
	slices.SortFunc(items, func(a, b types.TerminalInfo) int { return strings.Compare(a.Resource, b.Resource) })
	return items
}

func (h *Host) publishTerminalsLocked() {
	h.publish(types.RootResourceURI, types.StateAction{Value: &types.RootTerminalsChangedAction{Type: types.ActionTypeRootTerminalsChanged, Terminals: h.terminalsLocked()}}, nil, nil)
}

func (p *Project) terminalDataLocked(a *terminalChannel, data string) {
	if title := a.filter.title; title != "" {
		a.filter.title = ""
		if title != a.state.Title {
			action := types.StateAction{Value: &types.TerminalTitleChangedAction{Type: types.ActionTypeTerminalTitleChanged, Title: title}}
			sdk.ApplyActionToTerminal(&a.state, action)
			p.host.publish(a.resource, action, nil, nil)
			p.host.publishTerminalsLocked()
		}
	}
	if data == "" {
		return
	}
	action := types.StateAction{Value: &types.TerminalDataAction{Type: types.ActionTypeTerminalData, Data: data}}
	sdk.ApplyActionToTerminal(&a.state, action)
	p.host.publish(a.resource, action, nil, nil)
	if a.size += len(data); a.size > terminalMaxContent {
		// Only new subscribers see the shorter snapshot; current ones keep
		// rendering their own history, so no reset is broadcast.
		var content strings.Builder
		for _, part := range a.state.Content {
			if text, ok := part.Value.(*types.TerminalUnclassifiedPart); ok {
				content.WriteString(text.Value)
			}
		}
		tail := string(terminal.SafeTail([]byte(content.String()), terminalTrimmedContent))
		a.state.Content = []types.TerminalContentPart{{Value: &types.TerminalUnclassifiedPart{Type: "unclassified", Value: tail}}}
		a.size = len(tail)
	}
}

// AHP terminals are never unowned: a disconnected owner hands its terminals
// to the host, from which any client may claim them.
func (p *Project) releaseTerminalsLocked(clientID string) {
	for c := range p.host.clients {
		if c.id == clientID {
			return
		}
	}
	released := false
	for _, a := range p.terminalChannels {
		if claim, ok := a.state.Claim.Value.(*types.TerminalClientClaim); ok && claim.ClientId == clientID {
			action := types.StateAction{Value: &types.TerminalClaimedAction{Type: types.ActionTypeTerminalClaimed, Claim: hostTerminalClaim()}}
			sdk.ApplyActionToTerminal(&a.state, action)
			p.host.publish(a.resource, action, nil, nil)
			released = true
		}
	}
	if released {
		p.host.publishTerminalsLocked()
	}
}

func (c *connection) createTerminal(raw json.RawMessage) (any, error) {
	var p types.CreateTerminalParams
	if json.Unmarshal(raw, &p) != nil || !ahp.ValidTerminalURI(p.Channel) {
		return nil, ahp.InvalidParams("valid terminal channel required")
	}
	project, err := c.host.loadedProject(p.Cwd)
	if err != nil {
		return nil, err
	}
	owner, ok := p.Claim.Value.(*types.TerminalClientClaim)
	if !ok || owner.ClientId != c.id {
		return nil, ahp.InvalidParams("initial terminal claim must belong to the creating client")
	}
	if project.terminals == nil {
		return nil, ahp.InvalidParams("terminal service unavailable")
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
		return nil, ahp.InvalidParams("invalid shell")
	}
	cols, rows := int64(terminal.DefaultCols), int64(terminal.DefaultRows)
	if p.Cols != nil {
		cols = *p.Cols
	}
	if p.Rows != nil {
		rows = *p.Rows
	}
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return nil, ahp.InvalidParams("terminal dimensions must be between 1 and 1000")
	}
	project.host.terminalMu.Lock()
	defer project.host.terminalMu.Unlock()
	if project.host.terminal(p.Channel) != nil {
		return nil, &ahp.Error{Code: int(types.ErrorCodeAlreadyExists), Message: "terminal already exists"}
	}
	session, err := project.terminals.CreateShell(shell, dir, name, int(cols), int(rows))
	if err != nil {
		return nil, err
	}
	project.attachTerminal(session, p.Channel, p.Claim)
	return nil, nil
}

func (c *connection) ownsTerminalLocked(a *terminalChannel) bool {
	claim, ok := a.state.Claim.Value.(*types.TerminalClientClaim)
	return ok && claim.ClientId == c.id
}

func (c *connection) disposeTerminal(resource string) (any, error) {
	a := c.host.terminal(resource)
	if a == nil {
		return nil, &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "terminal not found"}
	}
	h := c.host
	a.opMu.Lock()
	defer a.opMu.Unlock()
	h.mu.Lock()
	if a.project.terminalChannels[resource] != a {
		h.mu.Unlock()
		return nil, &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "terminal not found"}
	}
	if !c.ownsTerminalLocked(a) {
		h.mu.Unlock()
		return nil, ahp.InvalidParams("claim the terminal before disposing it")
	}
	delete(a.project.terminalChannels, resource)
	h.publishTerminalsLocked()
	h.mu.Unlock()
	a.project.terminals.Remove(a.session.ID())
	return nil, nil
}

func (c *connection) executeTerminalAction(p types.DispatchActionParams) error {
	a := c.host.terminal(p.Channel)
	if a == nil {
		return &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "terminal not found"}
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
func (c *connection) applyTerminalAction(a *terminalChannel, p types.DispatchActionParams) ([]byte, error) {
	h := c.host
	a.opMu.Lock()
	defer a.opMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.project.terminalChannels[a.resource] != a {
		return nil, ahp.InvalidParams("terminal disposed")
	}
	owned := c.ownsTerminalLocked(a)
	if action, ok := p.Action.Value.(*types.TerminalClaimedAction); ok {
		claim, valid := action.Claim.Value.(*types.TerminalClientClaim)
		if !valid || claim.ClientId == "" {
			return nil, ahp.InvalidParams("client terminal claim required")
		}
		if !owned {
			old, clientOwned := a.state.Claim.Value.(*types.TerminalClientClaim)
			if !clientOwned || claim.ClientId != c.id {
				return nil, ahp.InvalidParams("terminal belongs to another owner")
			}
			for peer := range h.clients {
				if peer.id == old.ClientId {
					return nil, ahp.InvalidParams("terminal owner is still connected")
				}
			}
		}
	} else if !owned {
		return nil, ahp.InvalidParams("terminal belongs to another client")
	}
	switch action := p.Action.Value.(type) {
	case *types.TerminalInputAction:
		if len(action.Data) > 1<<20 || a.session.Exited() {
			return nil, ahp.InvalidParams("terminal input is unavailable")
		}
		return []byte(action.Data), nil
	case *types.TerminalResizedAction:
		if action.Cols < 1 || action.Rows < 1 || action.Cols > 1000 || action.Rows > 1000 {
			return nil, ahp.InvalidParams("invalid terminal dimensions")
		}
		h.mu.Unlock()
		err := a.session.Resize(int(action.Cols), int(action.Rows))
		h.mu.Lock()
		if err != nil {
			return nil, err
		}
		if a.project.terminalChannels[a.resource] != a {
			return nil, ahp.InvalidParams("terminal disposed")
		}
	case *types.TerminalTitleChangedAction:
		if strings.TrimSpace(action.Title) == "" || len(action.Title) > 1024 {
			return nil, ahp.InvalidParams("invalid terminal title")
		}
	case *types.TerminalClaimedAction, *types.TerminalClearedAction:
	default:
		return nil, ahp.InvalidParams("terminal action is server-only or unsupported")
	}
	sdk.ApplyActionToTerminal(&a.state, p.Action)
	switch p.Action.Value.(type) {
	case *types.TerminalTitleChangedAction, *types.TerminalClaimedAction:
		h.publishTerminalsLocked()
	case *types.TerminalClearedAction:
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
func (s *Project) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	uri, err := url.PathUnescape(chi.URLParam(r, "terminal"))
	if err != nil {
		http.Error(w, "invalid terminal URI", http.StatusBadRequest)
		return
	}
	h := s
	h.startTerminals()
	h.host.mu.Lock()
	terminal := h.terminalChannels[uri]
	h.host.mu.Unlock()
	if terminal == nil {
		http.Error(w, "terminal not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]bool{"busy": terminal.session.HasForegroundProcess()})
}
