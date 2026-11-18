package agents

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp/claude"
	"github.com/adrianliechti/wingman-agent/pkg/acp/codex"
	"github.com/adrianliechti/wingman-agent/pkg/acp/pi"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/acp"
	"github.com/adrianliechti/wingman-agent/pkg/external"
	claudecli "github.com/adrianliechti/wingman-agent/pkg/external/claude"
	codexcli "github.com/adrianliechti/wingman-agent/pkg/external/codex"
	picli "github.com/adrianliechti/wingman-agent/pkg/external/pi"
)

type Constructor func(context.Context, *code.Workspace) (code.Agent, error)

type Registration struct {
	ID          string
	Name        string
	Constructor Constructor
	// AllFolders says that one running agent lists the sessions of every
	// folder, so a host need not start it in each project to list them.
	AllFolders bool
}

func ID(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), "-")
}

func nativeClaudeOptions(root string, env []string) claude.Options {
	return claude.Options{Cwd: root, Env: env, Stderr: io.Discard}
}

func nativeCodexOptions(root string, env []string) codex.Options {
	return codex.Options{Dir: root, Env: env, Stderr: io.Discard}
}

func nativePiOptions(env []string) pi.Options {
	return pi.Options{
		Path:        picli.BinPath(),
		Env:         env,
		Stderr:      io.Discard,
		SessionsDir: picli.NativeSessionsDir(),
	}
}

type inProcessAgent interface {
	acpsdk.Agent
	SetAgentConnection(*acpsdk.AgentSideConnection)
	Close() error
}

// newExternal starts an external ACP agent. A start that fails returns no
// agent: a nil *acp.Agent in a code.Agent is not nil, and closing it would
// panic the host.
func newExternal(ctx context.Context, ws *code.Workspace, def code.AgentDef) (code.Agent, error) {
	a, err := acp.New(ctx, ws, def)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// inProcess registers an agent whose adapter runs in this process. Each of
// them reads its agent's own store, which holds the sessions of every folder.
func inProcess(id, name string, start func(context.Context, *code.Workspace) (inProcessAgent, error)) Registration {
	def := code.AgentDef{Name: id, AllFolders: true}
	return Registration{
		ID: id, Name: name, AllFolders: def.AllFolders,
		Constructor: func(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
			srv, err := start(ctx, ws)
			if err != nil {
				return nil, err
			}
			a, err := acp.NewInProcess(ctx, ws, def, srv, srv.SetAgentConnection, srv.Close)
			if err != nil {
				_ = srv.Close()
				return nil, err
			}
			return a, nil
		},
	}
}

func detected() []Registration {
	var out []Registration

	if _, err := exec.LookPath(claudecli.BinPath()); err == nil {
		out = append(out, inProcess("claude", "Claude", func(_ context.Context, ws *code.Workspace) (inProcessAgent, error) {
			return claude.New(nativeClaudeOptions(ws.RootPath, os.Environ())), nil
		}))
	}

	if _, err := exec.LookPath(codexcli.BinPath()); err == nil {
		out = append(out, inProcess("codex", "Codex", func(ctx context.Context, ws *code.Workspace) (inProcessAgent, error) {
			srv, err := codex.Spawn(ctx, nativeCodexOptions(ws.RootPath, os.Environ()))
			if err != nil {
				return nil, fmt.Errorf("codex spawn: %w", err)
			}
			return srv, nil
		}))
	}

	for _, def := range []code.AgentDef{
		// Copilot lists every folder's sessions when asked without one.
		{Name: "copilot", Command: external.LookupPath("copilot", "copilot"), Args: []string{"--acp", "--stdio"}, AllFolders: true},
		{Name: "opencode", Command: external.LookupPath("opencode", "opencode"), Args: []string{"acp"}},
	} {
		path, err := exec.LookPath(def.Command)
		if err != nil {
			continue
		}
		def.Command = path
		name := def.Name
		out = append(out, Registration{
			ID: ID(name), Name: name, AllFolders: def.AllFolders,
			Constructor: func(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
				return newExternal(ctx, ws, def)
			},
		})
	}

	if _, err := exec.LookPath(picli.BinPath()); err == nil {
		out = append(out, inProcess("pi", "Pi", func(context.Context, *code.Workspace) (inProcessAgent, error) {
			return pi.New(nativePiOptions(os.Environ())), nil
		}))
	}

	return out
}

func configured() ([]Registration, error) {
	defs, err := code.LoadAgents()
	if err != nil {
		return nil, err
	}
	out := make([]Registration, 0, len(defs))
	for _, def := range defs {
		id := ID(def.Name)
		if id == code.BuiltinAgentName {
			continue
		}
		name := def.Name
		def.Name = id
		out = append(out, Registration{
			ID: id, Name: name, AllFolders: def.AllFolders,
			Constructor: func(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
				return newExternal(ctx, ws, def)
			},
		})
	}
	return out, nil
}

// Available merges automatically detected agents with agents.json entries.
// An explicitly configured entry replaces a detected entry with the same ID.
func Available() ([]Registration, error) {
	out := detected()
	index := make(map[string]int, len(out))
	for i, r := range out {
		index[r.ID] = i
	}
	configured, err := configured()
	if err != nil {
		return nil, err
	}
	for _, r := range configured {
		if i, ok := index[r.ID]; ok {
			out[i] = r
			continue
		}
		index[r.ID] = len(out)
		out = append(out, r)
	}
	return out, nil
}

func names(registrations []Registration) []string {
	names := []string{code.BuiltinAgentName}
	for _, r := range registrations {
		names = append(names, r.ID)
	}
	return names
}

// ListsAllFolders reports whether an agent, once it runs anywhere, lists the
// sessions of every folder.
func ListsAllFolders(name string) bool {
	name = ID(name)
	available, err := Available()
	if err != nil {
		return false
	}
	for _, r := range available {
		if r.ID == name {
			return r.AllFolders
		}
	}
	return false
}

// NewExternal constructs a detected/configured ACP agent. Hosts construct the
// built-in code agent directly and own its provider resources.
func NewExternal(ctx context.Context, ws *code.Workspace, name string) (code.Agent, error) {
	name = ID(name)

	available, err := Available()
	if err != nil {
		return nil, err
	}
	for _, r := range available {
		if r.ID == name {
			return r.Constructor(ctx, ws)
		}
	}
	return nil, fmt.Errorf("unknown agent %q (available: %s)", name, strings.Join(names(available), ", "))
}
