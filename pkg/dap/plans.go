package dap

import (
	"time"

	"github.com/google/uuid"
)

const (
	reviewedPlanTTL   = 30 * time.Minute
	reviewedPlanLimit = 32
)

type reviewedPlan struct {
	value   any
	created time.Time
}

// RememberPlan keeps a host-generated launch plan so a later launch refers to
// it by id instead of trusting a client-supplied copy of its commands.
func (m *Manager) RememberPlan(plan any) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plans == nil {
		m.plans = make(map[string]reviewedPlan)
	}
	oldest := ""
	for id, plan := range m.plans {
		if time.Since(plan.created) > reviewedPlanTTL {
			delete(m.plans, id)
		} else if oldest == "" || plan.created.Before(m.plans[oldest].created) {
			oldest = id
		}
	}
	if len(m.plans) >= reviewedPlanLimit {
		delete(m.plans, oldest)
	}
	id := uuid.NewString()
	m.plans[id] = reviewedPlan{value: plan, created: time.Now()}
	return id
}

func (m *Manager) ReviewedPlan(id string) (any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	plan, ok := m.plans[id]
	if !ok || time.Since(plan.created) > reviewedPlanTTL {
		return nil, false
	}
	return plan.value, true
}
