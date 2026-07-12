package code

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) getPhase() AppPhase {
	return AppPhase(a.phase.Load())
}

func (a *App) applySpinnerForPhase(phase AppPhase) {
	if a.spinner == nil {
		return
	}
	if phase == PhaseIdle {
		a.spinner.Stop()
		a.updateInputHint()
	} else {
		a.spinner.Start(phase)
	}
}

func (a *App) setPhase(phase AppPhase) {
	a.phase.Store(int32(phase))
	a.applySpinnerForPhase(phase)
}

func (a *App) queuePhase(phase AppPhase) {
	a.phase.Store(int32(phase))
	// Active events may be emitted synchronously from Submit on the tview event
	// loop. QueueUpdateDraw blocks until that loop executes the callback, so it
	// must be scheduled from another goroutine here.
	go a.app.QueueUpdateDraw(func() {
		if a.getPhase() == phase {
			a.applySpinnerForPhase(phase)
		}
	})
}

const renderInterval = 40 * time.Millisecond

func (a *App) render() {
	if !a.renderPending.CompareAndSwap(false, true) {
		return
	}

	delay := renderInterval - time.Duration(time.Now().UnixNano()-a.renderLast.Load())
	if delay < 0 {
		delay = 0
	}

	time.AfterFunc(delay, func() {
		a.app.QueueUpdateDraw(func() {
			a.renderPending.Store(false)
			a.renderLast.Store(time.Now().UnixNano())

			if a.promptActive || a.askActive {
				return
			}

			a.renderChat(a.agent.Messages(a.sessionID))

			usage := a.agent.Usage(a.sessionID)
			a.inputTokens = usage.InputTokens
			a.cachedTokens = usage.CachedTokens
			a.outputTokens = usage.OutputTokens
			a.lastInputTokens = usage.LastInputTokens
			a.updateStatusBar()
		})
	})
}

func (a *App) clearStreamingState() {
	a.streamStateMu.Lock()
	a.streamingText = ""
	a.streamingReasoning = ""
	a.currentToolName = ""
	a.currentToolHint = ""
	a.reasoningID = ""
	a.streamStateMu.Unlock()
}

func (a *App) snapshotStreamState() (toolName, toolHint, text, reasoning string) {
	a.streamStateMu.Lock()
	defer a.streamStateMu.Unlock()
	return a.currentToolName, a.currentToolHint, a.streamingText, a.streamingReasoning
}

func (a *App) handleTurnEvent(ev code.TurnEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			visible := false
			var epoch uint64
			a.sessionMu.Lock()
			if a.sessionID == ev.SessionID {
				epoch = a.sessionEpoch
				a.clearStreamingState()
				a.queuePhase(PhaseIdle)
				visible = true
			}
			a.sessionMu.Unlock()
			if visible {
				a.app.QueueUpdateDraw(func() {
					if a.sessionID != ev.SessionID || a.sessionEpoch != epoch {
						return
					}
					fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Internal error: %v", recovered), theme.Default.Red))
					a.updateStatusBar()
				})
			}
		}
	}()

	if ev.Message != nil {
		a.withCurrentSession(ev.SessionID, func() {
			a.handleStreamMessage(*ev.Message)
		})
		return
	}

	switch ev.State {
	case code.TurnInputActive:
		a.withCurrentSession(ev.SessionID, func() {
			a.queuePhase(PhaseThinking)
		})
	case code.TurnInputCompleted, code.TurnInputCancelled, code.TurnInputFailed:
		commit := a.takeTurnCommit(ev.InputID)
		if ev.Executed {
			a.finishTurn(ev.SessionID, commit, ev.State, ev.Err)
		}
	}
}

func (a *App) handleStreamMessage(msg agent.Message) {
	for _, c := range msg.Content {
		switch {
		case c.ToolCall != nil:
			hint := tool.ExtractHint(c.ToolCall.Args, c.ToolCall.Name)
			a.streamStateMu.Lock()
			a.currentToolName = c.ToolCall.Name
			a.currentToolHint = hint
			a.streamingText = ""
			a.streamingReasoning = ""
			a.reasoningID = ""
			a.streamStateMu.Unlock()
			a.queuePhase(PhaseToolRunning)
			a.render()

		case c.ToolResult != nil:
			a.streamStateMu.Lock()
			a.currentToolName = ""
			a.currentToolHint = ""
			a.streamingText = ""
			a.streamStateMu.Unlock()

		case c.Reasoning != nil && c.Reasoning.Summary != "":
			if a.getPhase() != PhaseThinking {
				a.queuePhase(PhaseThinking)
			}
			a.streamStateMu.Lock()
			if a.reasoningID != "" && c.Reasoning.ID != a.reasoningID {
				a.streamingReasoning = ""
			}
			a.streamingReasoning += c.Reasoning.Summary
			a.reasoningID = c.Reasoning.ID
			a.streamStateMu.Unlock()
			a.render()

		case c.Text != "":
			if a.getPhase() != PhaseStreaming {
				a.queuePhase(PhaseStreaming)
			}
			a.streamStateMu.Lock()
			a.streamingReasoning = ""
			a.streamingText += c.Text
			a.reasoningID = ""
			a.streamStateMu.Unlock()
			a.render()
		}
	}
}

func (a *App) finishTurn(sessionID, commit string, state code.TurnInputState, turnErr error) {
	t := theme.Default
	var (
		epoch   uint64
		usage   agent.Usage
		visible bool
	)
	a.sessionMu.Lock()
	if a.sessionID == sessionID {
		epoch = a.sessionEpoch
		nextPhase := PhaseIdle
		for _, input := range a.turns.Snapshot(sessionID).Inputs {
			if input.State == code.TurnInputActive {
				nextPhase = PhaseThinking
				break
			}
		}
		a.queuePhase(nextPhase)
		usage = a.agent.Usage(sessionID)
		visible = true
	}
	a.sessionMu.Unlock()
	if visible {
		a.app.QueueUpdateDraw(func() {
			// Session changes and queued UI callbacks both run on the tview event
			// loop, so this generation check is race-free and cannot go stale
			// between the check and the render below.
			if a.sessionID != sessionID || a.sessionEpoch != epoch {
				return
			}
			a.inputTokens = usage.InputTokens
			a.cachedTokens = usage.CachedTokens
			a.outputTokens = usage.OutputTokens
			a.lastInputTokens = usage.LastInputTokens

			a.clearStreamingState()
			if state == code.TurnInputCompleted {
				a.renderChat(a.agent.Messages(sessionID))
			} else if state == code.TurnInputCancelled || errors.Is(turnErr, context.Canceled) {
				fmt.Fprint(a.chatView, a.formatNotice("Cancelled", t.Yellow))
			} else {
				fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Error: %v", turnErr), t.Red))
			}

			a.updateStatusBar()
		})
	}

	if state == code.TurnInputCompleted {
		a.commitRewind(commit)
		_ = a.agent.Save(sessionID)
	}
}

func (a *App) rememberTurn(id string, input []agent.Content) {
	commit := "<unknown>"
	for _, c := range input {
		if c.Text != "" {
			commit = c.Text
			break
		}
	}
	a.turnMu.Lock()
	a.turnCommits[id] = commit
	a.turnMu.Unlock()
}

func (a *App) takeTurnCommit(id string) string {
	a.turnMu.Lock()
	commit := a.turnCommits[id]
	delete(a.turnCommits, id)
	a.turnMu.Unlock()
	return commit
}
