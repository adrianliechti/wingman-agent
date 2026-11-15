package server

import (
	"context"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

func (s *Project) registerSessionRoutes(r chi.Router) {
	r.Route("/v2", func(r chi.Router) {
		r.Post("/backends/{backend}/models/refresh", s.backendOperation((*backendRuntime).handleRefreshModels))
		r.Route("/chats/{chat}", func(r chi.Router) {
			r.Get("/reviews", s.chatHandler((*sessionController).handleTurnReviews))
			r.Get("/reviews/{turnID}", s.chatHandler((*sessionController).handleTurnReviews))
			r.Post("/reviews/{turnID}/undo", s.chatHandler((*sessionController).handleTurnUndo))
			r.Get("/tasks", s.chatHandler((*sessionController).handleTasks))
			r.Get("/tasks/{taskID}", s.chatHandler((*sessionController).handleTask))
			r.Post("/tasks/{taskID}/stop", s.chatHandler((*sessionController).handleTaskStop))
			r.Get("/schedules", s.chatHandler((*sessionController).handleSchedules))
			r.Delete("/schedules/{scheduleID}", s.chatHandler((*sessionController).handleScheduleDelete))
			r.Post("/schedules/{scheduleID}/pause", s.chatHandler((*sessionController).handleSchedulePause))
			r.Post("/schedules/{scheduleID}/resume", s.chatHandler((*sessionController).handleScheduleResume))
		})
	})
}

// chatHandler resolves the chat once and passes its controller to the handler.
func (s *Project) chatHandler(fn func(*sessionController, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat, err := url.PathUnescape(chi.URLParam(r, "chat"))
		if err != nil {
			http.Error(w, "invalid chat URI", http.StatusBadRequest)
			return
		}
		a, ch, err := s.host.ensureSession(chat)
		// The host may serve other projects' chats too.
		if err != nil || ch == nil || a.project != s {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}
		c := ch.controller
		c.backend.serveOperation(w, r, func(w http.ResponseWriter, r *http.Request) { fn(c, w, r) })
	}
}

func (s *Project) backendOperation(fn func(*backendRuntime, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := s.backend(r.PathValue("backend"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		b.serveOperation(w, r, func(w http.ResponseWriter, r *http.Request) { fn(b, w, r) })
	}
}

// Requests keep their backend alive and end when either the client or project closes.
func (b *backendRuntime) serveOperation(w http.ResponseWriter, r *http.Request, fn http.HandlerFunc) {
	if !b.beginOperation() {
		http.Error(w, "workspace closed", http.StatusConflict)
		return
	}
	defer b.operations.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	fn(w, r.WithContext(ctx))
}
