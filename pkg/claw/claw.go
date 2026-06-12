package claw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/subagent"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/claw/memory"
	"github.com/adrianliechti/wingman-agent/pkg/claw/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/claw/tool/manage"
	"github.com/adrianliechti/wingman-agent/pkg/claw/tool/schedule"
)

type managedAgent struct {
	agent *agent.Agent

	// mu serializes runs and session saves for this agent
	mu sync.Mutex

	// notifyRoute is where scheduled reports go: the first conversation
	// that ever messaged this agent
	notifyRoute channel.Route

	// snapshot of the agent state after the last completed run,
	// readable without blocking on an active run
	snapMu       sync.Mutex
	snapMessages []agent.Message
	snapUsage    agent.Usage

	cancel  context.CancelFunc
	scratch string
}

func (ma *managedAgent) updateSnapshot() {
	ma.snapMu.Lock()
	ma.snapMessages = slices.Clone(ma.agent.Messages)
	ma.snapUsage = ma.agent.Usage
	ma.snapMu.Unlock()
}

type Claw struct {
	config *Config

	mu     sync.RWMutex
	agents map[string]*managedAgent
	runCtx context.Context

	mcpTools []tool.Tool
}

func New(config *Config) *Claw {
	return &Claw{config: config, agents: map[string]*managedAgent{}}
}

func (c *Claw) Init() error {
	if c.config.MCP != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := c.config.MCP.Connect(ctx); err != nil {
			log.Printf("warning: connect MCP servers: %v", err)
		}

		if tools, err := mcp.Tools(ctx, c.config.MCP); err == nil {
			c.mcpTools = tools
		} else {
			log.Printf("warning: load MCP tools: %v", err)
		}
	}

	names, err := c.config.Memory.ListAgents()
	if err != nil {
		return fmt.Errorf("failed to list agents: %w", err)
	}

	c.mu.Lock()
	for _, name := range names {
		if _, err := c.loadAgent(name); err != nil {
			log.Printf("warning: failed to load agent %q: %v", name, err)
		}
	}
	c.mu.Unlock()

	c.ensureDefaultTasks("main")

	return nil
}

func (c *Claw) Run(ctx context.Context) error {
	if len(c.config.Channels) == 0 {
		return fmt.Errorf("no channels configured")
	}

	c.runCtx = ctx

	c.mu.Lock()
	for name, ma := range c.agents {
		c.startScheduler(name, ma)
	}
	c.mu.Unlock()

	for _, ch := range c.config.Channels[:len(c.config.Channels)-1] {
		go func(ch channel.Channel) {
			if err := ch.Start(ctx, c.handleMessage); err != nil {
				log.Printf("channel %s: %v", ch.Name(), err)
			}
		}(ch)
	}

	primary := c.config.Channels[len(c.config.Channels)-1]
	return primary.Start(ctx, c.handleMessage)
}

func (c *Claw) Close() {
	c.mu.Lock()
	agents := c.agents
	c.agents = map[string]*managedAgent{}
	c.mu.Unlock()

	for _, ma := range agents {
		c.unloadAgent(ma)
	}
}

func (c *Claw) unloadAgent(ma *managedAgent) {
	if ma.cancel != nil {
		ma.cancel()
	}

	ma.mu.Lock()
	defer ma.mu.Unlock()

	if ma.scratch != "" {
		os.RemoveAll(ma.scratch)
	}
}

func (c *Claw) CreateAgent(name string) error {
	if err := memory.ValidName(name); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

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
		c.startScheduler(name, ma)
	}

	return nil
}

func (c *Claw) DeleteAgent(name string) error {
	if err := memory.ValidName(name); err != nil {
		return err
	}
	if name == "main" {
		return fmt.Errorf("cannot delete the main agent")
	}

	c.mu.Lock()
	ma := c.agents[name]
	delete(c.agents, name)
	c.mu.Unlock()

	if ma != nil {
		c.unloadAgent(ma)
	}

	return c.config.Memory.RemoveAgent(name)
}

func (c *Claw) ListAgents() ([]string, error) {
	return c.config.Memory.ListAgents()
}

func (c *Claw) agentWorkDir(name string) string {
	if name == "main" {
		return c.config.Memory.Dir()
	}
	return c.config.Memory.WorkspaceDir(name)
}

func (c *Claw) loadAgent(name string) (*managedAgent, error) {
	workDir := c.agentWorkDir(name)

	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace for agent %q: %w", name, err)
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open workspace for agent %q: %w", name, err)
	}

	cfg := c.config.AgentConfig.Derive()
	cfg.Instructions = func() string { return c.buildInstructions(name) }
	cfg.Hooks.PreToolUse = append(cfg.Hooks.PreToolUse, auditHook(name))

	scratchDir, err := os.MkdirTemp("", "claw-scratch-")
	if err != nil {
		return nil, fmt.Errorf("failed to create scratch directory for agent %q: %w", name, err)
	}
	cfg.Hooks.PostToolUse = append(cfg.Hooks.PostToolUse,
		truncation.New(scratchDir),
	)

	agentTools := slices.Concat(
		fs.Tools(root, &fs.Options{AllowedReadRoots: []string{scratchDir}}),
		shell.Tools(workDir, nil),
		c.config.Tools,
		c.mcpTools,
		schedule.Tools(c.config.Memory.AgentDir(name)),
	)

	if name == "main" {
		agentTools = append(agentTools, manage.Tools(c, c.config.Memory)...)
	}

	agentTools = append(agentTools, subagent.Tools(cfg, func() string { return c.buildAgentContext(name) })...)

	cfg.Tools = func() []tool.Tool { return agentTools }

	a := &agent.Agent{Config: cfg}

	sessionPath := c.sessionPath(name)

	var state agent.State
	if err := state.Load(sessionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		corrupt := sessionPath + ".corrupt"
		os.Rename(sessionPath, corrupt)
		log.Printf("agent %s: session unreadable, moved to %s: %v", name, corrupt, err)
		state = agent.State{}
	}

	a.Messages = state.Messages
	a.Usage = state.Usage

	ma := &managedAgent{
		agent:   a,
		scratch: scratchDir,
	}
	ma.updateSnapshot()
	c.agents[name] = ma
	return ma, nil
}

func (c *Claw) AgentState(name string) ([]agent.Message, agent.Usage, bool) {
	ma := c.getAgent(name)
	if ma == nil {
		return nil, agent.Usage{}, false
	}

	ma.snapMu.Lock()
	defer ma.snapMu.Unlock()
	return ma.snapMessages, ma.snapUsage, true
}

func (c *Claw) getAgent(name string) *managedAgent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.agents[name]
}

func (c *Claw) AgentDir(name string) string {
	return c.config.Memory.AgentDir(name)
}

func (c *Claw) handleMessage(ctx context.Context, msg channel.Message) {
	ch := c.findChannel(msg.Channel)
	if ch == nil {
		log.Printf("no channel %q for message to agent %q", msg.Channel, msg.Agent)
		return
	}

	if c.config.Authorize != nil && !c.config.Authorize(msg) {
		ch.Send(ctx, msg.Conversation, "Not authorized.")
		return
	}

	ma := c.getAgent(msg.Agent)
	if ma == nil {
		ch.Send(ctx, msg.Conversation, fmt.Sprintf("Agent %q is not registered. Use create_agent to create it.", msg.Agent))
		return
	}

	ma.mu.Lock()
	defer ma.mu.Unlock()

	if ma.notifyRoute == (channel.Route{}) {
		ma.notifyRoute = channel.Route{Channel: msg.Channel, Conversation: msg.Conversation}
	}

	var stream io.WriteCloser
	if s, ok := ch.(channel.Streamer); ok {
		w, err := s.SendStream(ctx, msg.Conversation)
		if err != nil {
			log.Printf("failed to open stream: %v", err)
			return
		}
		stream = w
		defer stream.Close()
	}

	var buf strings.Builder
	write := func(text string) {
		if stream != nil {
			stream.Write([]byte(text))
			return
		}
		buf.WriteString(text)
	}

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	turn := ma.agent.Send(ctx, []agent.Content{{Text: msg.Content}})
	if turn == nil {
		return
	}

	for m, err := range turn {
		if err != nil {
			write(fmt.Sprintf("\nerror: %v", err))
			break
		}

		for _, content := range m.Content {
			if content.Text != "" {
				write(content.Text)
			}
		}
	}

	if stream == nil && buf.Len() > 0 {
		if err := ch.Send(ctx, msg.Conversation, buf.String()); err != nil {
			log.Printf("agent %s: failed to deliver reply: %v", msg.Agent, err)
		}
	}

	c.saveSession(msg.Agent, ma)
	ma.updateSnapshot()
}

func (c *Claw) sessionPath(name string) string {
	return filepath.Join(c.config.Memory.AgentDir(name), "session.json")
}

func (c *Claw) saveSession(name string, ma *managedAgent) {
	if c.getAgent(name) != ma {
		return
	}

	state := agent.State{
		Messages: ma.agent.Messages,
		Usage:    ma.agent.Usage,
	}

	if err := state.Save(c.sessionPath(name)); err != nil {
		log.Printf("agent %s: failed to save session: %v", name, err)
	}
}

func (c *Claw) findChannel(name string) channel.Channel {
	for _, ch := range c.config.Channels {
		if ch.Name() == name {
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

	now := time.Now().Format("Monday, January 2, 2006 15:04 MST")

	var b strings.Builder

	if soul := c.config.Memory.SoulContent(name); soul != "" {
		b.WriteString(soul)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "You are %s (agent: %s).\n", assistantName, name)
	fmt.Fprintf(&b, "Current date and time: %s. Scheduled tasks (cron) run in this timezone.\n", now)
	fmt.Fprintf(&b, "Working directory: %s\n", c.agentWorkDir(name))

	b.WriteString("\n")
	b.WriteString(prompt.Instructions)

	if c.config.Instructions != "" {
		b.WriteString("\n\n")
		b.WriteString(c.config.Instructions)
	}

	if content := c.config.Memory.Content(name); content != "" {
		b.WriteString("\n\n")
		b.WriteString(content)
	}

	return b.String()
}

func (c *Claw) buildAgentContext(name string) string {
	var b strings.Builder

	b.WriteString("## Environment\n\n")
	fmt.Fprintf(&b, "- Current Date and Time: %s\n", time.Now().Format("Monday, January 2, 2006 15:04 MST"))
	fmt.Fprintf(&b, "- Working Directory: %s\n", c.agentWorkDir(name))
	b.WriteString("\nUse relative paths with the file tools; paths outside the working directory are rejected.")

	if content := c.config.Memory.Content(name); content != "" {
		b.WriteString("\n\n## Guidelines\n\n")
		b.WriteString(content)
	}

	return b.String()
}

func (c *Claw) ensureDefaultTasks(name string) {
	agentDir := c.config.Memory.AgentDir(name)

	if schedule.HasTaskFile(agentDir) {
		return
	}

	err := schedule.Mutate(agentDir, func(tasks []schedule.Task) ([]schedule.Task, error) {
		task, err := schedule.NewTask(
			"Check if there is anything you should proactively do. Review your workspace, check pending items, and report anything that needs attention.",
			"every 30m",
		)
		if err != nil {
			return nil, err
		}

		return append(tasks, task), nil
	})
	if err != nil {
		log.Printf("warning: ensure default task for %q: %v", name, err)
	}
}

func auditHook(agentName string) hook.PreToolUse {
	return func(_ context.Context, call tool.ToolCall) (string, error) {
		switch call.Name {
		case "shell", "web_fetch", "write", "edit":
			log.Printf("audit %s: %s %s", agentName, call.Name, tool.ExtractHint(call.Args, call.Name))
		}
		return "", nil
	}
}
