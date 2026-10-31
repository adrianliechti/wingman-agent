package dap

import (
	"fmt"
	"slices"
)

// A session with child sessions presents their threads, frames and variables
// in one id space, so each request reaches the child that owns the id.
type childRoute struct {
	child  *Session
	native int
}

type childRouteKey struct {
	kind   string
	child  *Session
	native int
}

type childRoutes struct {
	epoch  uint64 // frames and variables are only valid within one state
	next   int
	ids    map[childRouteKey]int
	routes map[string]map[int]childRoute
}

// Caller holds session.mu.
func (session *Session) childIDLocked(kind string, child *Session, native int) int {
	if native <= 0 {
		return native
	}
	r := &session.childRoutes
	if r.ids == nil {
		r.ids, r.routes = map[childRouteKey]int{}, map[string]map[int]childRoute{}
	}
	if kind != "thread" && r.epoch != session.stateVersion {
		for key := range r.ids {
			if key.kind != "thread" {
				delete(r.ids, key)
			}
		}
		delete(r.routes, "frame")
		delete(r.routes, "variable")
		r.epoch = session.stateVersion
	}
	key := childRouteKey{kind, child, native}
	if id, ok := r.ids[key]; ok {
		return id
	}
	r.next++
	r.ids[key] = r.next
	if r.routes[kind] == nil {
		r.routes[kind] = map[int]childRoute{}
	}
	r.routes[kind][r.next] = childRoute{child, native}
	return r.next
}

func (session *Session) childID(kind string, child *Session, native int) int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.childIDLocked(kind, child, native)
}

// routeChild finds the child that owns id. A missing id routes to the primary
// child, which owns the current stop.
func (session *Session) routeChild(kind string, id int) (*Session, int, error) {
	session.mu.Lock()
	route, ok := session.childRoutes.routes[kind][id]
	stale := kind != "thread" && session.childRoutes.epoch != session.stateVersion
	session.mu.Unlock()
	if id <= 0 {
		return session.primaryChild(), id, nil
	}
	if !ok || stale {
		if kind == "thread" {
			return session.primaryChild(), id, nil
		}
		return nil, 0, fmt.Errorf("debug %s %d is no longer valid", kind, id)
	}
	return route.child, route.native, nil
}

// threadChild returns the child that owns a thread, or nil without children.
func (session *Session) threadChild(threadID int) (*Session, int) {
	if session.childSession() == nil {
		return nil, 0
	}
	child, native, _ := session.routeChild("thread", threadID)
	return child, native
}

// primaryChild is the focused child, or a stopped one while the focused child
// runs, so the session stays inspectable at any child's stop.
func (session *Session) primaryChild() *Session {
	children, focused := session.childSessions(), session.childSession()
	if focused == nil || focused.Status().State == StateStopped {
		return focused
	}
	for _, child := range slices.Backward(children) {
		if child.Status().State == StateStopped {
			return child
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
}

func childLabel(child *Session) string {
	if name, _ := child.plan.Arguments["name"].(string); name != "" {
		return name
	}
	return child.id
}
