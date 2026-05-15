package claw

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"

	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/claw/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/claw/tool/manage"
	"github.com/adrianliechti/wingman-agent/pkg/claw/tool/schedule"
)

type managedAgent struct {
	name  string
	agent *agent.Agent
}

type Claw struct {
	config *Config
	agents sync.Map // name -> *managedAgent
	runCtx context.Context
}

func New(config *Config) *Claw {
	return &Claw{config: config}
}

// Init must be called before Run.
func (c *Claw) Init() error {
	names, err := c.config.Memory.ListAgents()
	if err != nil {
		return fmt.Errorf("failed to list agents: %w", err)
	}

	for _, name := range names {
		if _, err := c.loadAgent(name); err != nil {
			log.Printf("warning: failed to load agent %q: %v", name, err)
		}
	}

	c.ensureDefaultTasks("main")

	return nil
}

func (c *Claw) Run(ctx context.Context) error {
	if len(c.config.Channels) == 0 {
		return fmt.Errorf("no channels configured")
	}

	c.runCtx = ctx

	c.agents.Range(func(k, v any) bool {
		go c.startScheduler(ctx, k.(string), v.(*managedAgent))
		return true
	})

	errCh := make(chan error, len(c.config.Channels))

	for _, ch := range c.config.Channels[:len(c.config.Channels)-1] {
		go func(ch channel.Channel) {
			errCh <- ch.Start(ctx, c.handleMessage)
		}(ch)
	}

	primary := c.config.Channels[len(c.config.Channels)-1]
	return primary.Start(ctx, c.handleMessage)
}

func (c *Claw) CreateAgent(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\:*?\"<>|") || name == "global" {
		return fmt.Errorf("invalid agent name %q", name)
	}

	if c.config.Memory.AgentExists(name) {
		return fmt.Errorf("agent %q already exists", name)
	}

	if err := c.config.Memory.EnsureAgent(name); err != nil {
		return err
	}

	ma, err := c.loadAgent(name)
	if err != nil {
		return err
	}

	if c.runCtx != nil {
		go c.startScheduler(c.runCtx, name, ma)
	}

	return nil
}

func (c *Claw) DeleteAgent(name string) error {
	if name == "main" {
		return fmt.Errorf("cannot delete the main agent")
	}

	c.agents.LoadAndDelete(name)

	return c.config.Memory.RemoveAgent(name)
}

func (c *Claw) ListAgents() ([]string, error) {
	return c.config.Memory.ListAgents()
}

func (c *Claw) loadAgent(name string) (*managedAgent, error) {
	workDir := c.config.Memory.WorkspaceDir(name)
	if name == "main" {
		workDir = c.config.Memory.Dir()
	}

	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace for agent %q: %w", name, err)
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open workspace for agent %q: %w", name, err)
	}

	cfg := c.config.AgentConfig.Derive()
	cfg.Instructions = func() string { return c.buildInstructions(name) }

	// Cap large tool outputs at the wire layer; the hook saves the full text under .scratch
	// so the model can `read` the path to retrieve the elided middle.
	scratchDir := filepath.Join(workDir, ".scratch")
	_ = os.MkdirAll(scratchDir, 0755)
	cfg.Hooks.PostToolUse = append(cfg.Hooks.PostToolUse,
		truncation.New(scratchDir),
	)

	agentTools := slices.Concat(
		fs.Tools(root),
		shell.Tools(workDir, nil),
		c.config.Tools,
		schedule.Tools(c.config.Memory.AgentDir(name)),
	)

	if c.config.MCP != nil {
		if mcpTools, err := mcp.Tools(context.Background(), c.config.MCP); err == nil {
			agentTools = append(agentTools, mcpTools...)
		}
	}

	if name == "main" {
		agentTools = append(agentTools, manage.Tools(c, c.config.Memory)...)
	}

	// subagent filters itself out via name check
	agentTools = append(agentTools, subagent.Tools(cfg)...)

	cfg.Tools = func() []tool.Tool { return agentTools }

	a := &agent.Agent{Config: cfg}

	sessionPath := c.sessionPath(name)

	var state agent.State
	state.Load(sessionPath)

	a.Messages = state.Messages
	a.Usage = state.Usage

	ma := &managedAgent{
		name:  name,
		agent: a,
	}
	c.agents.Store(name, ma)
	return ma, nil
}

func (c *Claw) GetAgent(name string) *agent.Agent {
	if ma, ok := c.agents.Load(name); ok {
		return ma.(*managedAgent).agent
	}
	return nil
}

func (c *Claw) getAgent(chatID string) *managedAgent {
	name := nameFromChatID(chatID)
	if ma, ok := c.agents.Load(name); ok {
		return ma.(*managedAgent)
	}
	return nil
}

func (c *Claw) AgentDir(name string) string {
	return c.config.Memory.AgentDir(name)
}

func (c *Claw) handleMessage(ctx context.Context, msg channel.Message) {
	ch := c.findChannel(msg.ChatID)
	if ch == nil {
		log.Printf("no channel for chat %s", msg.ChatID)
		return
	}

	ma := c.getAgent(msg.ChatID)
	if ma == nil {
		name := nameFromChatID(msg.ChatID)
		ch.Send(ctx, msg.ChatID, fmt.Sprintf("Agent %q is not registered. Use create_agent to create it.", name))
		return
	}

	stream, err := ch.SendStream(ctx, msg.ChatID)
	if err != nil {
		log.Printf("failed to open stream: %v", err)
		return
	}
	defer stream.Close()

	input := []agent.Content{{Text: msg.Content}}
	name := nameFromChatID(msg.ChatID)

	for msg, err := range ma.agent.Send(ctx, input) {
		if err != nil {
			fmt.Fprintf(stream, "\nerror: %v", err)
			break
		}

		for _, content := range msg.Content {
			if content.Text != "" {
				stream.Write([]byte(content.Text))
			}
		}
	}

	c.saveSession(name, ma)
}

func (c *Claw) sessionPath(name string) string {
	return filepath.Join(c.config.Memory.AgentDir(name), "session.json")
}

func (c *Claw) saveSession(name string, ma *managedAgent) {
	state := agent.State{
		Messages: ma.agent.Messages,
		Usage:    ma.agent.Usage,
	}

	state.Save(c.sessionPath(name))
}

func (c *Claw) findChannel(chatID string) channel.Channel {
	prefix, _, _ := strings.Cut(chatID, ":")
	for _, ch := range c.config.Channels {
		if ch.Name() == prefix {
			return ch
		}
	}
	return nil
}

func (c *Claw) buildInstructions(name string) string {
	assistantName := c.config.AssistantName
	if assistantName == "" {
		assistantName = "Claw"
	}

	now := time.Now().Format("January 2, 2006")

	var b strings.Builder

	// SOUL.md is immutable identity, outside workspace so the agent cannot modify it
	if soul := c.config.Memory.SoulContent(name); soul != "" {
		b.WriteString(soul)
		b.WriteString("\n\n")
	}

	workDir := c.config.Memory.WorkspaceDir(name)
	if name == "main" {
		workDir = c.config.Memory.Dir()
	}

	fmt.Fprintf(&b, "You are %s (agent: %s).\n", assistantName, name)
	fmt.Fprintf(&b, "Today's date is %s.\n", now)
	fmt.Fprintf(&b, "Working directory: %s\n", workDir)

	b.WriteString("\n")
	b.WriteString(prompt.Instructions)

	if c.config.Instructions != "" {
		b.WriteString("\n\n")
		b.WriteString(c.config.Instructions)
	}

	// AGENTS.md is mutable instructions the agent can modify
	if content := c.config.Memory.Content(name); content != "" {
		b.WriteString("\n\n")
		b.WriteString(content)
	}

	return b.String()
}

func (c *Claw) ensureDefaultTasks(name string) {
	agentDir := c.config.Memory.AgentDir(name)
	tasks := schedule.LoadTasks(agentDir)

	if len(tasks) > 0 {
		return
	}

	defaultTasks := []schedule.Task{
		{
			ID:        uuid.NewString(),
			Prompt:    "Check if there is anything you should proactively do. Review your workspace, check pending items, and report anything that needs attention.",
			Schedule:  "every 30m",
			Status:    "active",
			CreatedAt: time.Now().UTC(),
		},
	}

	schedule.SaveTasks(agentDir, defaultTasks)
}

func nameFromChatID(chatID string) string {
	if _, name, ok := strings.Cut(chatID, ":"); ok {
		return name
	}
	return chatID
}
