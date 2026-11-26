package code

import (
	"context"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
)

type modelFetcher interface {
	FetchModels(context.Context) error
}

type sessionSaver interface {
	Save(string) error
}

type recapProvider interface {
	Recap(context.Context, string) (string, error)
}

type sideQuestioner interface {
	Ask(ctx context.Context, sessionID, question string, onText func(string)) (string, error)
}

type contextStatsProvider interface {
	ContextStats(string) (harness.ContextStats, bool)
}

type taskProvider interface {
	Tasks(string) *task.Registry
	RunningTaskCount() int
}

type scheduleProvider interface {
	Schedules(string) schedule.Store
}

type toolProvider interface {
	Tools(string) []tool.Tool
}

type userShellRecorder interface {
	RecordUserShellCommand(sessionID, command, output string, exitCode int, duration time.Duration) error
}

type uiAwareAgent interface {
	SetUI(code.UI)
}

func setAgentUI(a code.Agent, ui code.UI) {
	if aware, ok := a.(uiAwareAgent); ok {
		aware.SetUI(ui)
	}
}
