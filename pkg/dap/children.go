package dap

import (
	"errors"
	"fmt"
	"slices"
)

// A session with child sessions presents their threads, frames and variables
// in one id space, so each request reaches the child that owns the id.
type childRoute struct {
	child  *Session
	epoch  uint64 // the child's state version; frames and variables expire with it
	native int
}

type childRouteKey struct {
	kind   string
	child  *Session
	epoch  uint64
	native int
}

type childRoutes struct {
	next   int
	ids    map[childRouteKey]int
	routes map[string]map[int]childRoute
	epochs map[*Session]uint64
}

// Caller holds session.mu. Threads outlive stops and ignore epoch.
func (session *Session) childIDLocked(kind string, child *Session, epoch uint64, native int) int {
	if native <= 0 {
		return native
	}
	r := &session.childRoutes
	if r.ids == nil {
		r.ids, r.routes, r.epochs = map[childRouteKey]int{}, map[string]map[int]childRoute{}, map[*Session]uint64{}
	}
	if kind == "thread" {
		epoch = 0
	} else if epoch > r.epochs[child] {
		for key, id := range r.ids {
			if key.child == child && key.kind != "thread" && key.epoch < epoch {
				delete(r.ids, key)
				delete(r.routes[key.kind], id)
			}
		}
		r.epochs[child] = epoch
	}
	key := childRouteKey{kind, child, epoch, native}
	if id, ok := r.ids[key]; ok {
		return id
	}
	r.next++
	r.ids[key] = r.next
	if r.routes[kind] == nil {
		r.routes[kind] = map[int]childRoute{}
	}
	r.routes[kind][r.next] = childRoute{child, epoch, native}
	return r.next
}

func (session *Session) childID(kind string, child *Session, epoch uint64, native int) int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.childIDLocked(kind, child, epoch, native)
}

// routeChild finds the child that owns id. A missing id routes to the primary
// child, which owns the current stop.
func (session *Session) routeChild(kind string, id int) (childRoute, error) {
	if id <= 0 {
		child := session.primaryChild()
		if child == nil {
			return childRoute{}, errors.New("debug session has no child session")
		}
		return childRoute{child, child.stateEpoch(), id}, nil
	}
	session.mu.Lock()
	route, ok := session.childRoutes.routes[kind][id]
	session.mu.Unlock()
	if !ok || kind != "thread" && route.child.stateEpoch() != route.epoch {
		return childRoute{}, fmt.Errorf("debug %s %d is no longer valid", kind, id)
	}
	if kind == "thread" {
		route.epoch = route.child.stateEpoch()
	}
	return route, nil
}

// threadChild returns the child that owns a thread, or none without children.
func (session *Session) threadChild(threadID int) (childRoute, error) {
	if session.childSession() == nil {
		return childRoute{}, nil
	}
	return session.routeChild("thread", threadID)
}

// primaryChild is the focused child, or a stopped one while the focused child
// runs, or a live one once the focused child has ended.
func (session *Session) primaryChild() *Session {
	children, focused := session.childSessions(), session.childSession()
	if focused == nil {
		return nil
	}
	state := focused.Status().State
	if state == StateStopped {
		return focused
	}
	for _, child := range slices.Backward(children) {
		if child.Status().State == StateStopped {
			return child
		}
	}
	if state == StateTerminated {
		for _, child := range slices.Backward(children) {
			if child.Status().State != StateTerminated {
				return child
			}
		}
	}
	return focused
}

func (session *Session) forgetChildRoutes(child *Session) {
	r := &session.childRoutes
	for key, id := range r.ids {
		if key.child == child {
			delete(r.ids, key)
			delete(r.routes[key.kind], id)
		}
	}
	delete(r.epochs, child)
}

func childLabel(child *Session) string {
	if name, _ := child.plan.Arguments["name"].(string); name != "" {
		return name
	}
	return child.id
}
