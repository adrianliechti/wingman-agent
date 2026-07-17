package code

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/clipboard"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

var _ code.UI = (*App)(nil)

type App struct {
	ctx   context.Context
	agent *coder.Agent
	term  *inline.Terminal

	queue    chan func()
	quit     chan struct{}
	quitOnce sync.Once
	runErr   error

	sessionMu    sync.Mutex
	sessionID    string
	sessionEpoch uint64

	phase      atomic.Int32
	phaseStart time.Time

	spinnerFrame int

	currentMode Mode
	showWelcome bool

	editor  *Editor
	popup   *Popup
	overlay Overlay

	selecting bool
	selAnchor selPos
	selHead   selPos
	selActive bool

	chat          []string
	chatScroll    int
	follow        bool
	lastChatRows  int
	lastMaxScroll int
	lastTopPad    int

	printed     int
	prevWasTool bool

	turnTools    int
	turnThoughts int
	turnStart    time.Time

	pendingEcho []pendingEchoItem

	elicitMu       sync.Mutex
	promptActive   bool
	promptResponse chan bool
	confirmAll     atomic.Bool
	askActive      bool
	askResponse    chan string

	inputTokens     int64
	cachedTokens    int64
	outputTokens    int64
	lastInputTokens int64

	pendingContent []agent.Content
	pendingFiles   []string

	turns       *code.TurnManager
	turnMu      sync.Mutex
	turnCommits map[string]string

	renderPending atomic.Bool
	renderLast    atomic.Int64
	dirty         bool

	streamStateMu      sync.Mutex
	currentToolID      string
	currentToolName    string
	currentToolHint    string
	streamingText      string
	streamingReasoning string
	reasoningID        string
}

type pendingEchoItem struct {
	ID   string
	Text string
}

func New(ctx context.Context, coderAgent *coder.Agent, sessionID string) *App {
	saveExecutablePath()

	hasMessages := sessionID != "" && len(coderAgent.Messages(sessionID)) > 0

	a := &App{
		ctx:   ctx,
		agent: coderAgent,
		term:  inline.NewTerminal(),

		queue: make(chan func(), 64),
		quit:  make(chan struct{}),

		sessionID:    sessionID,
		sessionEpoch: 1,
		showWelcome:  !hasMessages && os.Getenv("WINGMAN_CALLER") != "vscode",

		editor:      NewEditor(),
		turnCommits: map[string]string{},
		follow:      true,
	}

	a.turns = code.NewTurnManager(ctx, coderAgent, a.handleTurnEvent)

	return a
}

// WithTerminal replaces the terminal, used by tests.
func (a *App) WithTerminal(t *inline.Terminal) {
	a.term = t
}

func (a *App) SetSessionID(id string) {
	a.sessionMu.Lock()
	a.sessionID = id
	a.sessionEpoch++
	a.sessionMu.Unlock()
}

// activateSession changes the session and resets all state that belongs to
// the previous turn. The epoch prevents already-queued UI callbacks from an
// older activation of the same session from rendering later.
func (a *App) activateSession(id string) {
	a.sessionMu.Lock()
	a.sessionID = id
	a.sessionEpoch++
	a.clearStreamingState()
	a.setPhase(PhaseIdle)
	a.sessionMu.Unlock()

	a.printed = 0
	a.prevWasTool = false
	a.turnTools = 0
	a.turnThoughts = 0
}

func (a *App) withCurrentSession(id string, fn func()) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.sessionID == id {
		fn()
	}
}

func saveExecutablePath() {
	path := os.Getenv("WINGMAN_PATH")

	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return
		}

		path = exe
	}

	if path == "" {
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	dir := filepath.Join(home, ".wingman")
	os.MkdirAll(dir, 0755)

	os.WriteFile(filepath.Join(dir, "path"), []byte(path), 0644)
}

// post schedules fn on the UI loop from any goroutine.
func (a *App) post(fn func()) {
	select {
	case a.queue <- fn:
	case <-a.quit:
	}
}

func (a *App) invalidate() {
	a.dirty = true
}

func (a *App) stop() {
	a.quitOnce.Do(func() {
		close(a.quit)
	})
}

func (a *App) saveSession() {
	_ = a.agent.Save(a.sessionID)
}

func (a *App) Run() error {
	if err := a.term.Start(); err != nil {
		return err
	}

	a.term.EnterAlt()
	a.term.EnableMouse(true)

	a.agent.FetchModels(a.ctx)

	a.setPhase(PhasePreparing)

	go func() {
		a.agent.Workspace().WarmUp()

		if err := a.agent.Workspace().InitMCP(a.ctx); err != nil {
			a.post(func() {
				a.appendChat(cellError("MCP initialization failed", err.Error(), a.width()))
			})
		}

		a.post(func() {
			a.setPhase(PhaseIdle)
			if !a.agent.Workspace().HasRewind() {
				a.appendChat(cellNotice(
					"Limited mode: working dir is too large for full features. Diffs, checkpoints, and code intelligence are disabled.",
					theme.Default.Yellow, a.width(),
				))
			}
			a.invalidate()
		})
	}()

	if messages := a.agent.Messages(a.sessionID); len(messages) > 0 {
		usage := a.agent.Usage(a.sessionID)
		a.inputTokens = usage.InputTokens
		a.cachedTokens = usage.CachedTokens
		a.outputTokens = usage.OutputTokens
		a.lastInputTokens = usage.LastInputTokens
		a.syncMessages()
	}

	a.invalidate()
	a.render()

	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.quit:
			a.shutdown()
			return a.runErr

		case <-a.ctx.Done():
			a.shutdown()
			return a.runErr

		case ev := <-a.term.Events():
			a.handleEvent(ev)

		case fn := <-a.queue:
			fn()

		case <-ticker.C:
			if a.getPhase() != PhaseIdle {
				a.spinnerFrame++
				a.invalidate()
			}
		}

		// Drain whatever queued up before painting once.
		for {
			select {
			case fn := <-a.queue:
				fn()
				continue
			default:
			}
			break
		}

		if a.dirty {
			a.dirty = false
			a.syncMessages()
			a.render()
		}
	}
}

func (a *App) shutdown() {
	a.saveSession()

	a.turns.SetHandler(nil)
	a.turns.Close()
	a.agent.Close()
	a.agent.Workspace().Close()

	a.term.Stop()

	if len(a.agent.Messages(a.sessionID)) > 0 {
		usage := a.agent.Usage(a.sessionID)
		fmt.Fprintf(os.Stderr, "\n")
		if usage.CachedTokens > 0 {
			fmt.Fprintf(os.Stderr, "  Tokens: ↑%s (%s cached) ↓%s\n", formatTokens(usage.InputTokens), formatTokens(usage.CachedTokens), formatTokens(usage.OutputTokens))
		} else {
			fmt.Fprintf(os.Stderr, "  Tokens: ↑%s ↓%s\n", formatTokens(usage.InputTokens), formatTokens(usage.OutputTokens))
		}
		fmt.Fprintf(os.Stderr, "  Resume: wingman --resume %s\n", a.sessionID)
		fmt.Fprintf(os.Stderr, "\n")
	}
}

func (a *App) width() int {
	w, _ := a.term.Size()
	if w <= 0 {
		return 80
	}
	return w
}

// appendChat adds finalized cells to the scrollable chat buffer.
func (a *App) appendChat(lines []string) {
	a.chat = append(a.chat, lines...)
	if len(lines) > 0 {
		a.showWelcome = false
	}
	a.invalidate()
}

// rebuildChat re-renders the whole chat buffer from the message history, used
// on resize and when toggling verbose rendering. Turn counters are preserved.
func (a *App) rebuildChat() {
	tools, thoughts := a.turnTools, a.turnThoughts

	a.chat = nil
	a.printed = 0
	a.prevWasTool = false
	a.clearSelection()
	a.syncMessages()

	a.turnTools, a.turnThoughts = tools, thoughts
	a.invalidate()
}

type selPos struct {
	Line int
	Col  int
}

func (p selPos) before(q selPos) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Col < q.Col)
}

// handleMouse routes wheel to chat scrolling and left-button drags to
// text selection; the two coexist without a mode switch.
func (a *App) handleMouse(ev inline.MouseEvent) {
	switch ev.Kind {
	case inline.MouseWheel:
		a.scrollChat(ev.WheelDelta * 3)

	case inline.MousePress:
		a.clearSelection()
		row := ev.Y - 1
		line := a.chatScroll + row - a.lastTopPad
		if row >= 0 && row < a.lastChatRows && line >= 0 && !a.showWelcome {
			a.selecting = true
			a.selAnchor = selPos{Line: line, Col: ev.X - 1}
			a.selHead = a.selAnchor
		}
		a.invalidate()

	case inline.MouseDrag:
		if !a.selecting {
			return
		}
		row := ev.Y - 1
		if row < 0 {
			row = 0
		}
		if row >= a.lastChatRows {
			row = a.lastChatRows - 1
		}
		line := a.chatScroll + row - a.lastTopPad
		if line < 0 {
			line = 0
		}
		a.selHead = selPos{Line: line, Col: ev.X - 1}
		a.selActive = true
		a.invalidate()

	case inline.MouseRelease:
		if a.selecting {
			a.selecting = false
			if a.selActive {
				a.copySelection()
			}
		}
	}
}

func (a *App) clearSelection() {
	a.selecting = false
	a.selActive = false
}

func (a *App) orderedSelection() (selPos, selPos) {
	if a.selHead.before(a.selAnchor) {
		return a.selHead, a.selAnchor
	}
	return a.selAnchor, a.selHead
}

// removePendingEcho drops the queued-input preview for id.
func (a *App) removePendingEcho(id string) {
	for i, item := range a.pendingEcho {
		if item.ID == id {
			a.pendingEcho = append(a.pendingEcho[:i], a.pendingEcho[i+1:]...)
			return
		}
	}
}

// chatViewLines composes the scrollable chat content: committed cells, the
// live streaming tail, and previews of inputs still queued behind the active
// turn.
func (a *App) chatViewLines(width int) []string {
	view := a.chat

	stream := a.streamCells(width)

	if len(stream) > 0 || len(a.pendingEcho) > 0 {
		view = append(append([]string(nil), a.chat...), stream...)
		for _, item := range a.pendingEcho {
			view = append(view, cellIndent+dim(ansi.Truncate("› "+item.Text, width-10, "…")+" (queued)"))
		}
	}

	return view
}

// copySelection extracts the selected plain text and puts it on the
// clipboard, silently.
func (a *App) copySelection() {
	start, end := a.orderedSelection()
	view := a.chatViewLines(a.width())

	var parts []string
	for l := start.Line; l <= end.Line && l < len(view); l++ {
		from, to := 0, int(^uint(0)>>1)
		if l == start.Line {
			from = start.Col
		}
		if l == end.Line {
			to = end.Col + 1
		}
		parts = append(parts, strings.TrimRight(ansi.CutPlain(view[l], from, to), " "))
	}

	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}

	go func() {
		if err := clipboard.WriteText(text); err != nil {
			a.post(func() {
				a.appendChat(cellNotice(fmt.Sprintf("Clipboard copy failed: %v", err), theme.Default.Red, a.width()))
			})
		}
	}()
}

// scrollChat adjusts the chat viewport; render() clamps and re-engages
// follow mode when the bottom is reached.
func (a *App) scrollChat(delta int) {
	if delta < 0 && a.follow {
		a.chatScroll = a.lastMaxScroll
	}
	a.follow = false
	a.chatScroll += delta
	if a.chatScroll < 0 {
		a.chatScroll = 0
	}
	a.invalidate()
}

func (a *App) isStreaming() bool {
	return a.getPhase() != PhaseIdle
}

func (a *App) handleEvent(ev inline.Event) {
	switch ev := ev.(type) {
	case inline.ResizeEvent:
		a.term.Resized(ev.Width, ev.Height)
		a.rebuildChat()

	case inline.MouseEvent:
		if a.overlay != nil {
			if m, ok := a.overlay.(interface{ HandleMouse(inline.MouseEvent) }); ok {
				m.HandleMouse(ev)
				a.invalidate()
			}
			return
		}
		a.handleMouse(ev)

	case inline.PasteEvent:
		a.handlePaste(ev.Text)
		a.invalidate()

	case inline.KeyEvent:
		a.handleKey(ev)
		a.invalidate()
	}
}

func (a *App) handlePaste(text string) {
	if a.overlay != nil {
		return
	}

	paths := detectFilePaths(text, a.agent.Workspace().RootPath)
	if len(paths) > 0 {
		for _, p := range paths {
			a.addFileToContext(normalizeFilePath(p, a.agent.Workspace().RootPath))
		}
		return
	}

	a.editor.Insert(strings.ReplaceAll(text, "\r\n", "\n"))
	a.syncCommandPopup()
}

func (a *App) handleKey(ev inline.KeyEvent) {
	if a.overlay != nil {
		if a.overlay.HandleKey(ev) {
			a.closeOverlay()
		}
		return
	}

	if a.popup != nil {
		if a.handlePopupKey(ev) {
			return
		}
	}

	switch ev.Key {
	case inline.KeyEsc:
		if a.isStreaming() {
			a.cancelStream()
			return
		}
		a.editor.SetText("")
		a.clearPendingContent()
		a.syncCommandPopup()
		return

	case inline.KeyCtrl:
		switch ev.Rune {
		case 'c':
			if a.isStreaming() {
				a.cancelStream()
				return
			}
			a.stop()
			return
		case 'o':
			a.showTranscript()
			return
		case 'y':
			a.copyLastResponse()
			return
		case 'l':
			a.clearChat()
			return
		case 'v':
			a.pasteFromClipboard()
			return
		}

	case inline.KeyTab:
		if !a.isStreaming() && a.popup == nil {
			a.toggleMode()
			return
		}

	case inline.KeyBacktab:
		if !a.isStreaming() {
			a.cycleModel()
			return
		}

	case inline.KeyEnter:
		if a.promptActive || a.askActive {
			a.answerPrompt()
			return
		}
		a.submitInput()
		return

	case inline.KeyUp:
		if a.editor.HandleKey(ev) {
			return
		}
		a.editor.HistoryPrev()
		return

	case inline.KeyDown:
		if a.editor.HandleKey(ev) {
			return
		}
		a.editor.HistoryNext()
		return

	case inline.KeyPgUp:
		a.scrollChat(-max(1, a.lastChatRows-1))
		return

	case inline.KeyPgDn:
		a.scrollChat(max(1, a.lastChatRows-1))
		return
	}

	if a.promptActive {
		a.handlePromptKey(ev)
		return
	}

	if ev.Key == inline.KeyRune && ev.Rune == '@' && !ev.Alt && a.popup == nil && !a.isStreaming() {
		a.showFilePicker()
		return
	}

	if a.editor.HandleKey(ev) {
		a.syncCommandPopup()
	}
}

// handlePopupKey routes keys to the active popup; returns true when consumed.
func (a *App) handlePopupKey(ev inline.KeyEvent) bool {
	popup := a.popup

	if popup.kind == popupCommands {
		switch ev.Key {
		case inline.KeyTab:
			if item, ok := popup.Current(); ok {
				a.editor.SetText(item.ID)
				a.syncCommandPopup()
			}
			return true
		case inline.KeyEnter:
			if item, ok := popup.Current(); ok && a.editor.Text() != item.ID && !strings.HasPrefix(a.editor.Text(), item.ID+" ") {
				a.editor.SetText(item.ID)
			}
			a.closePopup()
			a.submitInput()
			return true
		case inline.KeyEsc:
			a.closePopup()
			return true
		case inline.KeyUp, inline.KeyDown, inline.KeyPgUp, inline.KeyPgDn:
			consumed, _ := popup.HandleKey(ev)
			return consumed
		}
		return false
	}

	consumed, closed := popup.HandleKey(ev)
	if closed {
		a.closePopup()
	}
	return consumed
}

func (a *App) closePopup() {
	a.popup = nil
}

func (a *App) answerPrompt() {
	if a.askActive {
		text := strings.TrimSpace(a.editor.Text())
		if text == "" {
			return
		}
		a.editor.SetText("")
		a.appendChat(cellUser(text, a.width()))
		a.setPhase(PhaseThinking)
		a.askResponse <- text
	}
}

func (a *App) handlePromptKey(ev inline.KeyEvent) {
	if ev.Key != inline.KeyRune {
		return
	}

	switch ev.Rune {
	case 'y', 'Y':
		a.appendChat(cellUser("Yes", a.width()))
		a.setPhase(PhaseThinking)
		a.promptResponse <- true

	case 'n', 'N':
		a.appendChat(cellUser("No", a.width()))
		a.setPhase(PhaseThinking)
		a.promptResponse <- false

	case 'a', 'A':
		a.confirmAll.Store(true)
		a.appendChat(cellUser("Always", a.width()))
		a.appendChat(cellNotice("Auto-approving commands for this session", theme.Default.BrBlack, a.width()))
		a.setPhase(PhaseThinking)
		a.promptResponse <- true
	}
}

func (a *App) cancelStream() {
	a.turns.CancelAll(a.sessionID)

	if a.askActive {
		a.editor.SetText("")

		select {
		case a.askResponse <- "":
		default:
		}
	}

	if a.promptActive {
		select {
		case a.promptResponse <- false:
		default:
		}
	}
}

func (a *App) clearPendingContent() {
	a.pendingContent = nil
	a.pendingFiles = nil
}

func (a *App) countPendingImages() int {
	count := 0

	for _, c := range a.pendingContent {
		if c.File != nil {
			count++
		}
	}

	return count
}

func (a *App) clearChat() {
	previousID := a.sessionID
	id, err := a.agent.NewSession(a.ctx)
	if err != nil {
		a.appendChat(cellNotice(fmt.Sprintf("Could not create session: %v", err), theme.Default.Red, a.width()))
		return
	}
	a.turns.CancelAll(previousID)
	a.activateSession(id)
	a.clearPendingContent()
	a.inputTokens = 0
	a.cachedTokens = 0
	a.outputTokens = 0
	a.lastInputTokens = 0
	a.chat = nil
	a.chatScroll = 0
	a.follow = true
	a.clearSelection()
	a.invalidate()
}

func (a *App) resumeSession() {
	t := theme.Default

	sessions, err := a.agent.ListSessions(a.ctx)
	if err != nil || len(sessions) == 0 {
		a.appendChat(cellNotice("No sessions to resume", t.Yellow, a.width()))
		return
	}

	last := sessions[0]
	if err := a.agent.LoadSession(a.ctx, last.ID); err != nil {
		a.appendChat(cellNotice(fmt.Sprintf("Failed to load session: %v", err), t.Red, a.width()))
		return
	}

	a.turns.CancelAll(a.sessionID)
	a.activateSession(last.ID)
	a.clearPendingContent()

	usage := a.agent.Usage(a.sessionID)
	a.inputTokens = usage.InputTokens
	a.cachedTokens = usage.CachedTokens
	a.outputTokens = usage.OutputTokens
	a.lastInputTokens = usage.LastInputTokens

	a.showWelcome = false
	a.chat = nil
	a.chatScroll = 0
	a.follow = true
	a.clearSelection()
	a.syncMessages()
	a.appendChat(cellNotice(fmt.Sprintf("Resumed session from %s", last.UpdatedAt.Format("Jan 2 15:04")), t.Green, a.width()))
}

func (a *App) copyTextToClipboard(text string) {
	go func() {
		err := clipboard.WriteText(text)

		a.post(func() {
			message := "Copied to clipboard"
			color := theme.Default.BrBlack

			if err != nil {
				message = fmt.Sprintf("Clipboard copy failed: %v", err)
				color = theme.Default.Red
			}

			a.appendChat(cellNotice(message, color, a.width()))
		})
	}()
}

func (a *App) copyLastResponse() {
	messages := a.agent.Messages(a.sessionID)

	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agent.RoleAssistant {
			for _, c := range messages[i].Content {
				if c.Text != "" {
					a.copyTextToClipboard(c.Text)

					return
				}
			}
		}
	}
}

func (a *App) pasteFromClipboard() {
	go func() {
		contents, err := clipboard.Read()

		if err != nil || len(contents) == 0 {
			return
		}

		a.post(func() {
			for _, c := range contents {
				if c.Image != nil {
					a.pendingContent = append(a.pendingContent, agent.Content{File: &agent.File{Data: *c.Image}})
				}

				if c.Text != "" {
					paths := detectFilePaths(c.Text, a.agent.Workspace().RootPath)
					if len(paths) > 0 {
						for _, p := range paths {
							a.addFileToContext(normalizeFilePath(p, a.agent.Workspace().RootPath))
						}

						continue
					}

					a.editor.Insert(c.Text)
					a.syncCommandPopup()
				}
			}

			a.invalidate()
		})
	}()
}

func (a *App) showError(title string, err error) {
	a.appendChat(cellError(title, err.Error(), a.width()))
}

func (a *App) isToolHidden(name string) bool {
	for _, t := range a.agent.Tools(a.sessionID) {
		if t.Name == name {
			return t.Hidden
		}
	}

	return false
}

func (a *App) toggleMode() {
	if a.currentMode == ModeAgent {
		a.enterPlanMode()
		return
	}

	a.exitPlanMode()
}

func (a *App) enterPlanMode() {
	if a.currentMode == ModePlan {
		return
	}

	_ = a.agent.SetMode(a.ctx, a.sessionID, "plan")
	a.currentMode = ModePlan
}

func (a *App) exitPlanMode() {
	if a.currentMode == ModeAgent {
		return
	}

	_ = a.agent.SetMode(a.ctx, a.sessionID, "agent")
	a.currentMode = ModeAgent
}

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}

	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}

	return fmt.Sprintf("%d", n)
}
