package mcp

import "errors"

type State string

const (
	StatePending       State = "pending"
	StateConnecting    State = "connecting"
	StateConnected     State = "connected"
	StateFailed        State = "failed"
	StateUnapproved    State = "unapproved"
	StateLoginRequired State = "login_required"
	StateDisconnected  State = "disconnected"
)

type Status struct {
	State State
	Error string
}

// Status reports how far a configured server got; one never tried is pending.
func (m *Manager) Status(name string) Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if status, ok := m.status[name]; ok {
		return status
	}
	return Status{State: StatePending}
}

func (m *Manager) SetStatusChangedHandler(handler func()) {
	if handler == nil {
		m.statusChanged.Store(nil)
		return
	}
	m.statusChanged.Store(&handler)
}

func (m *Manager) setStatus(name string, state State, err error) {
	status := Status{State: state}
	if err != nil {
		status.Error = Redact(err).Error()
	}

	m.mu.Lock()
	changed := m.status[name] != status
	m.status[name] = status
	m.mu.Unlock()

	if !changed {
		return
	}
	if handler := m.statusChanged.Load(); handler != nil {
		(*handler)()
	}
}

func (m *Manager) setFailed(name string, err error) {
	state := StateFailed
	if errors.Is(err, ErrLoginRequired) {
		state = StateLoginRequired
	}
	m.setStatus(name, state, err)
}
