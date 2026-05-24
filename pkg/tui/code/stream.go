package code

import (
	"context"
	"errors"
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) getPhase() AppPhase {
	return AppPhase(a.phase.Load())
}

// UI goroutine only — Spinner mutates tview state.
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

// UI goroutine only.
func (a *App) setPhase(phase AppPhase) {
	a.phase.Store(int32(phase))
	a.applySpinnerForPhase(phase)
}

// queuePhase stores the phase synchronously (visible to cancel/command
// guards immediately) and queues the spinner update. The closure
// re-reads the phase so a later change supersedes a stale spinner update.
func (a *App) queuePhase(phase AppPhase) {
	a.phase.Store(int32(phase))
	a.app.QueueUpdateDraw(func() {
		if a.getPhase() != phase {
			return
		}
		a.applySpinnerForPhase(phase)
	})
}

// Messages is captured before the closure to avoid a race with
// a.agent.Messages(sid) being mutated mid-flight.
func (a *App) render() {
	if a.promptActive || a.askActive {
		return
	}

	messages := a.agent.Messages(a.sessionID)
	a.app.QueueUpdateDraw(func() {
		a.renderChat(messages)
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

// snapshotStreamState returns a consistent copy of the four streaming
// display fields for the UI goroutine to render against.
func (a *App) snapshotStreamState() (toolName, toolHint, text, reasoning string) {
	a.streamStateMu.Lock()
	defer a.streamStateMu.Unlock()
	return a.currentToolName, a.currentToolHint, a.streamingText, a.streamingReasoning
}

func (a *App) streamResponse(input []agent.Content) {
	t := theme.Default

	streamCtx, cancel := context.WithCancel(a.ctx)

	// Send returns nil if a turn is already running for this agent — the
	// input was queued onto it and the in-flight loop will pick it up at
	// its next safe boundary. Bail out without touching streamCancel /
	// phase / commit, since the active stream owns those.
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
				// Skip render: rapid tool call/result pairs would flash empty state.

			case c.Reasoning != nil && c.Reasoning.Summary != "":
				if a.getPhase() != PhaseThinking {
					a.queuePhase(PhaseThinking)
				}
				a.streamStateMu.Lock()
				// New reasoning item id: drop the prior in-progress block — it'll
				// reappear from agent.Messages once the request completes.
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

		usage := a.agent.Usage(a.sessionID)
		a.app.QueueUpdateDraw(func() {
			a.inputTokens = usage.InputTokens
			a.cachedTokens = usage.CachedTokens
			a.outputTokens = usage.OutputTokens
			a.updateStatusBar()
		})
	}

	a.queuePhase(PhaseIdle)

	a.app.QueueUpdateDraw(func() {
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
