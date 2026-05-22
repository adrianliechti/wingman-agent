// Package code defines the [Agent] interface implemented by the
// in-process wingman backend (sub-package wingman) and the external ACP
// subprocess backend (sub-package acp). Higher layers (server, TUI) hold
// one active Agent and swap it on user selection.
//
// The interface is session-id-explicit: every per-conversation operation
// takes a session id allocated via [Agent.NewSession]. Backends own the
// session catalog (wingman: on-disk; acp: the external server). Switching
// the active agent closes the prior one.
package code

import (
	"context"
	"errors"
	"iter"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
)

// ErrNotSupported signals that the active backend doesn't implement an
// optional operation (e.g. ACP servers have no DeleteSession RPC).
// Server / TUI use it to hide affordances and skip mid-flight errors.
var ErrNotSupported = errors.New("operation not supported by this agent")

// SessionInfo is one row in the backend's session catalog (wingman's
// on-disk list or the ACP server's ListSessions response).
type SessionInfo struct {
	ID        string
	Title     string
	UpdatedAt time.Time
}

// Agent is the swappable backend that drives chat turns and owns its
// own session catalog + transcript storage. The two concrete
// implementations live in sub-packages wingman and acp.
//
// All methods are safe for concurrent use across distinct session ids.
// Concurrent calls on the SAME session id are NOT supported — callers
// must ensure only one Send is in flight per session at a time.
type Agent interface {
	// Name returns a stable identifier for this backend. For wingman
	// it's [BuiltinAgentName]; for ACP backends it's the [AgentDef.Name]
	// that constructed them.
	Name() string

	// Workspace returns the shared workspace the agent is rooted at.
	// Same instance regardless of which backend is active — its
	// Rewind / LSP / MCP subsystems are wingman-only side-effects but
	// the workspace itself (RootPath, Skills, MemoryPath) is shared.
	Workspace() *Workspace

	// Models reports the available model catalog and the currently
	// selected id. Cached read — no RPC. Empty list = the backend
	// doesn't expose a model selector.
	Models() (available []Model, current string)

	// SetModel switches the active model. Returns [ErrNotSupported]
	// when the backend has no model selector.
	SetModel(ctx context.Context, id string) error

	// Effort reports the current effort id and the available values.
	// Empty options = effort selector not supported.
	Effort() (current string, options []string)

	SetEffort(ctx context.Context, value string) error

	// ListSessions returns the backend's saved session catalog scoped
	// to the workspace.
	ListSessions(ctx context.Context) ([]SessionInfo, error)

	// NewSession allocates a fresh session and returns its id. For
	// wingman this is a locally minted UUID; for ACP it's the id
	// returned by the ACP server's session/new.
	NewSession(ctx context.Context) (string, error)

	// LoadSession restores an existing session into memory so
	// [Messages] / [Usage] reflect its transcript and subsequent
	// [Send] calls continue it.
	LoadSession(ctx context.Context, id string) error

	// DeleteSession removes a session from the catalog. Returns
	// [ErrNotSupported] when the backend can't delete (ACP).
	DeleteSession(ctx context.Context, id string) error

	// Messages returns the transcript for a session id. Returns nil
	// for sessions the backend hasn't loaded.
	Messages(sessionID string) []agent.Message

	Usage(sessionID string) agent.Usage

	// Send drives a turn. Returns nil when a turn is already running
	// for this session id. The iterator yields per-update chunks; the
	// implementation commits the assembled message(s) into the
	// session's transcript by end-of-turn.
	Send(ctx context.Context, sessionID string, input []agent.Content) iter.Seq2[agent.Message, error]

	// Cancel aborts an in-flight Send for the session id. No-op when
	// no turn is in flight or the id is unknown.
	Cancel(sessionID string)

	// Close releases all resources (subprocess, IO, file handles).
	// Idempotent.
	Close() error
}
