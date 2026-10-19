package agent

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
)

func (s *sessionState) scheduleRunner() *schedule.Runner {
	return &schedule.Runner{
		Store: s.schedules,
		Gate: func(ctx context.Context, t schedule.Task) (bool, string, error) {
			if t.Script == "" {
				return true, "", nil
			}
			return schedule.RunTaskGate(code.WithSessionID(ctx, s.id), s.parent.workspace.RootPath, t, s.scriptOptions)
		},
		Deliver: func(_ context.Context, fire schedule.Fire) {
			prompt := fire.Task.Prompt
			if fire.GateOutput != "" {
				prompt += "\n\nPre-check script output:\n\n" + fire.GateOutput
			}
			s.resources.tasks.Publish(task.Event{
				ID:          fire.Task.ID,
				Kind:        task.KindSchedule,
				AgentType:   "schedule",
				Description: fire.Task.Schedule,
				Result:      prompt,
				Status:      task.StatusDone,
				// UIs derive input IDs from Seq to deduplicate delivery.
				Seq: int(fire.Sequence),
			})
		},
	}
}
