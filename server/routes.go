package server

import (
	"context"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

func (s *Server) registerSessionRoutes(r chi.Router) {
	r.Route("/v2", func(r chi.Router) {
		r.Post("/backends/{backend}/models/refresh", s.backendOperation(handleRefreshModels))
		r.Get("/backends/{backend}/transcription", s.backendOperation(handleTranscriptionStatus))
		r.Post("/backends/{backend}/transcription", s.backendOperation(handleTranscribe))
		r.Route("/chats/{chat}", func(r chi.Router) {
			r.Get("/reviews", s.chatHandler(handleTurnReviews))
			r.Get("/reviews/{turnID}", s.chatHandler(handleTurnReviews))
			r.Post("/reviews/{turnID}/undo", s.chatHandler(handleTurnUndo))
			r.Get("/tasks", s.chatHandler(handleTasks))
			r.Get("/tasks/{taskID}", s.chatHandler(handleTask))
			r.Post("/tasks/{taskID}/stop", s.chatHandler(handleTaskStop))
			r.Get("/schedules", s.chatHandler(handleSchedules))
			r.Delete("/schedules/{scheduleID}", s.chatHandler(handleScheduleDelete))
			r.Post("/schedules/{scheduleID}/pause", s.chatHandler(handleSchedulePause))
			r.Post("/schedules/{scheduleID}/resume", s.chatHandler(handleScheduleResume))
		})
	})
}

// chatHandler resolves the chat once and passes its controller to the handler.
func (s *Server) chatHandler(fn func(*sessionController, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chat, err := url.PathUnescape(chi.URLParam(r, "chat"))
		if err != nil {
			http.Error(w, "invalid chat URI", http.StatusBadRequest)
			return
		}
		a, ch, err := s.Project.host.ensureSession(chat)
		// The host may serve other projects' chats too.
		if err != nil || ch == nil || a.project != s.Project {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}
		c := ch.controller
		serveOperation(c.backend, w, r, func(w http.ResponseWriter, r *http.Request) { fn(c, w, r) })
	}
}

func (s *Server) backendOperation(fn func(*backendRuntime, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := s.Project.backend(r.PathValue("backend"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		serveOperation(b, w, r, func(w http.ResponseWriter, r *http.Request) { fn(b, w, r) })
	}
}

// Requests keep their backend alive and end when either the client or project closes.
func serveOperation(b *backendRuntime, w http.ResponseWriter, r *http.Request, fn http.HandlerFunc) {
	if !b.Project.beginOperation() {
		http.Error(w, "workspace closed", http.StatusConflict)
		return
	}
	defer b.Project.operations.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(b.Project.ctx, cancel)
	defer func() { stop(); cancel() }()
	fn(w, r.WithContext(ctx))
}
