package claw

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/claw/tool/schedule"
)

const (
	schedulerOK   = "SCHEDULER_OK"
	schedulerTick = 1 * time.Minute

	runTimeout = 1 * time.Hour
)

func (c *Claw) startScheduler(name string, ma *managedAgent) {
	ctx, cancel := context.WithCancel(c.runCtx)
	ma.cancel = cancel

	go func() {
		c.tickScheduler(ctx, name, ma)

		ticker := time.NewTicker(schedulerTick)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.tickScheduler(ctx, name, ma)
			}
		}
	}()
}

func (c *Claw) tickScheduler(ctx context.Context, name string, ma *managedAgent) {
	agentDir := c.config.Memory.AgentDir(name)

	tasks, err := schedule.List(agentDir)
	if err != nil {
		log.Printf("scheduler %s: failed to load tasks: %v", name, err)
		return
	}

	now := time.Now()

	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		if schedule.IsDue(t, now) {
			c.runTask(ctx, name, ma, agentDir, t)
		}
	}
}

func (c *Claw) runTask(ctx context.Context, name string, ma *managedAgent, agentDir string, t schedule.Task) {
	prompt := fmt.Sprintf(
		"Scheduled task is due:\n\n%s\n\nExecute the task. If nothing needs attention, reply with exactly: SCHEDULER_OK",
		t.Prompt,
	)

	ok := c.runScheduledTask(ctx, name, ma, prompt)
	if !ok {
		log.Printf("scheduler %s: task %s failed; retrying with backoff", name, t.ID)
	}

	now := time.Now()

	err := schedule.Mutate(agentDir, func(tasks []schedule.Task) ([]schedule.Task, error) {
		var kept []schedule.Task
		for i := range tasks {
			if tasks[i].ID == t.ID {
				if ok {
					if schedule.IsOneTime(tasks[i].Schedule) {
						continue
					}
					tasks[i].LastRun = &now
					tasks[i].Failures = 0
					tasks[i].LastAttempt = nil
				} else {
					tasks[i].Failures++
					tasks[i].LastAttempt = &now
				}
			}
			kept = append(kept, tasks[i])
		}
		return kept, nil
	})
	if err != nil {
		log.Printf("scheduler %s: failed to save tasks: %v", name, err)
	}
}

func (c *Claw) runScheduledTask(ctx context.Context, name string, ma *managedAgent, prompt string) bool {
	ma.mu.Lock()
	defer ma.mu.Unlock()

	checkpoint := len(ma.agent.Messages)
	revision := ma.agent.Revision

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	stream := ma.agent.Send(ctx, []agent.Content{{Text: prompt}})
	if stream == nil {
		log.Printf("scheduler %s: agent busy, skipping run", name)
		return false
	}

	rollback := func() {
		if ma.agent.Revision == revision && len(ma.agent.Messages) >= checkpoint {
			ma.agent.Messages = ma.agent.Messages[:checkpoint]
		}
	}

	var result strings.Builder

	for msg, err := range stream {
		if err != nil {
			log.Printf("scheduler %s: error: %v", name, err)
			rollback()
			return false
		}

		for _, content := range msg.Content {
			if content.Text != "" {
				result.WriteString(content.Text)
			}
		}
	}

	text := strings.TrimSpace(result.String())
	text = strings.TrimSpace(strings.TrimPrefix(text, schedulerOK))

	if text == "" {
		rollback()
		return true
	}

	c.saveSession(name, ma)
	ma.updateSnapshot()

	route := ma.notifyRoute
	if route == (channel.Route{}) {
		primary := c.config.Channels[len(c.config.Channels)-1]
		route = channel.Route{Channel: primary.Name(), Conversation: name}
	}

	if ch := c.findChannel(route.Channel); ch != nil {
		if err := ch.Send(ctx, route.Conversation, text); err != nil {
			log.Printf("scheduler %s: failed to deliver report to %s/%s: %v (kept in session history)", name, route.Channel, route.Conversation, err)
		}
	} else {
		log.Printf("scheduler %s: no channel %q; report kept in session history", name, route.Channel)
	}

	return true
}
