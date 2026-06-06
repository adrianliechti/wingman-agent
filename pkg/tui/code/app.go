package code

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/tui"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

var _ code.UI = (*App)(nil)

type App struct {
	ctx   context.Context
	app   *tview.Application
	agent *coder.Agent

	pages       *tview.Pages
	chatView    *tview.TextView
	welcomeView *tview.TextView
	input       *tview.TextArea
	statusBar   *tview.TextView
	inputHint   *tview.TextView

	contentPages  *tview.Flex
	chatContainer *tview.Flex
	inputSection  *tview.Flex
	inputFrame    *tview.Frame
	mainLayout    *tview.Flex

	spinner *Spinner

	sessionID string

	phase       atomic.Int32
	currentMode Mode
	showWelcome bool
	activeModal Modal

	elicitMu       sync.Mutex
	promptActive   bool
	promptResponse chan bool
	confirmAll     atomic.Bool
	askActive      bool
	askResponse    chan string

	expandLevel    int
	inputTokens    int64
	cachedTokens   int64
	outputTokens   int64
	chatWidth      int
	lastCompact    bool
	pendingContent []agent.Content
	pendingFiles   []string

	streamCancel context.CancelFunc
	streamMu     sync.Mutex

	renderPending atomic.Bool
	renderLast    atomic.Int64

	streamStateMu      sync.Mutex
	currentToolName    string
	currentToolHint    string
	streamingText      string
	streamingReasoning string

	lspTracker *lsp.DiagnosticTracker

	mouseEnabled bool
}

func New(ctx context.Context, agent *coder.Agent, sessionID string) *App {
	saveExecutablePath()

	hasMessages := sessionID != "" && len(agent.Messages(sessionID)) > 0

	a := &App{
		ctx:   ctx,
		app:   tview.NewApplication(),
		agent: agent,

		sessionID:   sessionID,
		showWelcome: !hasMessages && os.Getenv("WINGMAN_CALLER") != "vscode",

		lspTracker: lsp.NewDiagnosticTracker(),

		mouseEnabled: true,
	}

	return a
}

func (a *App) SetSessionID(id string) {
	a.sessionID = id
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

func (a *App) saveSession() {
	_ = a.agent.Save(a.sessionID)
}

func (a *App) stop() {
	a.saveSession()

	a.agent.Close()
	a.agent.Workspace().Close()

	a.app.EnableMouse(false)
	a.app.Stop()

	fmt.Fprint(os.Stdout, "\033[?1000l\033[?1002l\033[?1003l\033[?1006l")

	if len(a.agent.Messages(a.sessionID)) > 0 {
		usage := a.agent.Usage(a.sessionID)
		fmt.Fprintf(os.Stderr, "\n")
		if usage.CachedTokens > 0 {
			fmt.Fprintf(os.Stderr, "  Tokens: \u2191%s (%s cached) \u2193%s\n", tui.FormatTokens(usage.InputTokens), tui.FormatTokens(usage.CachedTokens), tui.FormatTokens(usage.OutputTokens))
		} else {
			fmt.Fprintf(os.Stderr, "  Tokens: \u2191%s \u2193%s\n", tui.FormatTokens(usage.InputTokens), tui.FormatTokens(usage.OutputTokens))
		}
		fmt.Fprintf(os.Stderr, "  Resume: wingman --resume %s\n", a.sessionID)
		fmt.Fprintf(os.Stderr, "\n")
	}
}

func (a *App) Run() error {
	a.setupUI()

	a.agent.FetchModels(a.ctx)

	mainLayout := a.buildLayout()
	a.spinner = NewSpinner(a.app, a.inputHint)
	a.pages = tview.NewPages()
	a.pages.SetBackgroundColor(tcell.ColorDefault)
	a.pages.AddPage("main", mainLayout, true, true)

	a.setPhase(PhasePreparing)

	go func() {
		a.agent.Workspace().WarmUp()

		if err := a.agent.Workspace().InitMCP(a.ctx); err != nil {
			a.app.QueueUpdateDraw(func() {
				a.showError("MCP initialization failed", err)
			})
		}

		a.app.QueueUpdateDraw(func() {
			a.setPhase(PhaseIdle)
			if a.agent.Workspace().Rewind == nil {
				t := theme.Default
				fmt.Fprint(a.chatView, a.formatNotice(
					"Limited mode: working dir is too large for full features. Diffs, checkpoints, and code intelligence are disabled.",
					t.Yellow,
				))
			}
			a.updateStatusBar()
		})
	}()

	if messages := a.agent.Messages(a.sessionID); len(messages) > 0 {
		a.switchToChat()
		a.renderChat(messages)

		usage := a.agent.Usage(a.sessionID)
		a.inputTokens = usage.InputTokens
		a.cachedTokens = usage.CachedTokens
		a.outputTokens = usage.OutputTokens
		a.updateStatusBar()
	}

	root := &pasteInterceptRoot{
		Primitive: a.pages,

		intercept: func(text string) bool {
			paths := detectFilePaths(text, a.agent.Workspace().RootPath)

			if len(paths) == 0 {
				return false
			}

			for _, p := range paths {
				a.addFileToContext(normalizeFilePath(p, a.agent.Workspace().RootPath))
			}

			a.updateInputHint()

			return true
		},
	}

	return a.app.SetRoot(root, true).EnableMouse(a.mouseEnabled).EnablePaste(true).Run()
}

func (a *App) toggleMode() {
	if a.currentMode == ModeAgent {
		a.enterPlanMode()
		return
	}

	a.exitPlanMode()
}

func (a *App) hasActiveModal() bool {
	return a.activeModal != ModalNone
}

func (a *App) closeActiveModal() {
	switch a.activeModal {
	case ModalPicker:
		a.closePicker()
	case ModalFilePicker:
		a.closeFilePicker()
	case ModalDiff:
		a.closeDiffView()
	case ModalDiagnostics:
		a.closeDiagnosticsView()
	}
}

func (a *App) isToolHidden(name string) bool {
	for _, t := range a.agent.Tools(a.sessionID) {
		if t.Name == name {
			return t.Hidden
		}
	}

	return false
}
