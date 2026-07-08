package code

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
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
	a.app.QueueUpdateDraw(func() {
		if a.getPhase() != phase {
			return
		}
		a.applySpinnerForPhase(phase)
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
	a.streamStateMu.Unlock()
}

func (a *App) snapshotStreamState() (toolName, toolHint, text, reasoning string) {
	a.streamStateMu.Lock()
	defer a.streamStateMu.Unlock()
	return a.currentToolName, a.currentToolHint, a.streamingText, a.streamingReasoning
}

func (a *App) streamResponse(input []agent.Content) {
	t := theme.Default

	streamCtx, cancel := context.WithCancel(a.ctx)

	stream := a.agent.Send(streamCtx, a.sessionID, input)
	if stream == nil {
		cancel()
		return
	}

	a.streamMu.Lock()
	a.streamCancel = cancel
	a.streamMu.Unlock()

	defer func() {
		a.streamMu.Lock()
		a.streamCancel = nil
		a.streamMu.Unlock()
	}()

	defer func() {
		if r := recover(); r != nil {
			a.clearStreamingState()
			a.queuePhase(PhaseIdle)
			a.app.QueueUpdateDraw(func() {
				fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Internal error: %v", r), t.Red))
				a.updateStatusBar()
			})
		}
	}()

	var reasoningID string
	var streamErr error

	a.queuePhase(PhaseThinking)

	for msg, err := range stream {
		if err != nil {
			streamErr = err
			break
		}

		for _, c := range msg.Content {
			switch {
			case c.ToolCall != nil:
				a.streamStateMu.Lock()
				a.currentToolName = c.ToolCall.Name
				a.currentToolHint = tool.ExtractHint(c.ToolCall.Args, c.ToolCall.Name)
				a.streamingText = ""
				a.streamingReasoning = ""
				a.streamStateMu.Unlock()
				reasoningID = ""
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

				if reasoningID != "" && c.Reasoning.ID != reasoningID {
					a.streamingReasoning = ""
				}
				a.streamingReasoning += c.Reasoning.Summary
				a.streamStateMu.Unlock()
				reasoningID = c.Reasoning.ID
				a.render()

			case c.Text != "":
				if a.getPhase() != PhaseStreaming {
					a.queuePhase(PhaseStreaming)
				}
				a.streamStateMu.Lock()
				a.streamingReasoning = ""
				a.streamingText += c.Text
				a.streamStateMu.Unlock()
				reasoningID = ""
				a.render()
			}
		}
	}

	a.queuePhase(PhaseIdle)

	usage := a.agent.Usage(a.sessionID)
	a.app.QueueUpdateDraw(func() {
		a.inputTokens = usage.InputTokens
		a.cachedTokens = usage.CachedTokens
		a.outputTokens = usage.OutputTokens
		a.lastInputTokens = usage.LastInputTokens

		if streamErr != nil {
			a.clearStreamingState()
			if errors.Is(streamErr, context.Canceled) {
				fmt.Fprint(a.chatView, a.formatNotice("Cancelled", t.Yellow))
			} else {
				fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Error: %v", streamErr), t.Red))
			}
		} else {
			a.clearStreamingState()
			a.renderChat(a.agent.Messages(a.sessionID))
		}

		a.updateStatusBar()
	})

	if streamErr == nil {
		var commit string

		for _, c := range input {
			if c.Text != "" {
				commit = c.Text
				break
			}
		}

		if commit == "" {
			commit = "<unknown>"
		}

		a.commitRewind(commit)
		a.saveSession()
	}
}
