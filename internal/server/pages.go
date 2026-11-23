package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// pagePage shows one chain's page: what it is about, who it has told, and
// a button that acknowledges it. Every chain message links here.
func (s *Server) pagePage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, http.StatusOK, "")
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, status int, done string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Page", "There is no such page.")
		return
	}
	ctx := r.Context()
	p, err := s.store.Page(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "Page", "There is no such page.")
		return
	}
	if s.pageFailed(w, r, err, "Page") {
		return
	}
	e, err := s.store.Event(ctx, p.EventID)
	if s.pageFailed(w, r, err, "Page") {
		return
	}
	n := s.namer(r)
	if viewer := door.From(ctx).Email; viewer != "" {
		if err := s.store.Opened(ctx, p.ID, viewer, time.Now()); err != nil {
			s.logRequest(ctx, slog.LevelError, "page not marked opened", r, "error", err)
		}
	}
	messages, err := s.store.PageMessages(ctx, p.ID)
	if s.pageFailed(w, r, err, "Page") {
		return
	}
	s.render(w, r, status, "page", map[string]any{"Page": p, "Event": e, "Steps": n.steps(p.Steps), "Chain": nameOr(n.chains, p.Chain), "Messages": messages, "Done": done})
}

func (s *Server) formAcknowledgePage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Page", "There is no such page.")
		return
	}
	box := s.desk.Notices()
	if box == nil {
		s.message(w, r, http.StatusServiceUnavailable, "Page", "This desk sends no notices.")
		return
	}
	by := door.From(r.Context()).Email
	ended, err := box.Acknowledge(r.Context(), id, by)
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "Page", "There is no such page.")
		return
	}
	if s.pageFailed(w, r, err, "Page") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "page acknowledge asked", r, "page", id, "ended", ended, "by", by)
	done := "You have it. Nobody else in the chain is told."
	if !ended {
		done = "This page had already ended."
	}
	s.renderPage(w, r, http.StatusOK, done)
}

// pageLine is a page as the Notifications page lists it.
type pageLine struct {
	notice.Page
	Title, ChainName string
}

func (s *Server) recentPages(r *http.Request) []pageLine {
	pages, err := s.store.RecentPages(r.Context(), 20)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "pages not read", r, "error", err)
		return nil
	}
	n := s.namer(r)
	out := make([]pageLine, 0, len(pages))
	for _, p := range pages {
		l := pageLine{Page: p, ChainName: nameOr(n.chains, p.Chain)}
		if e, err := s.store.Event(r.Context(), p.EventID); err == nil {
			l.Title = e.Title
		}
		out = append(out, l)
	}
	return out
}

// formEscalatePage tells a page's next step now, as the person on the desk.
func (s *Server) formEscalatePage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Page", "There is no such page.")
		return
	}
	box := s.desk.Notices()
	if box == nil {
		s.message(w, r, http.StatusServiceUnavailable, "Page", "This desk sends no notices.")
		return
	}
	moved, err := box.Escalate(r.Context(), id, door.From(r.Context()).Email)
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "Page", "There is no such page.")
		return
	}
	if s.pageFailed(w, r, err, "Page") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "page escalate asked", r, "page", id, "moved", moved)
	done := "The next step is told now."
	if !moved {
		done = "This page had already ended."
	}
	s.renderPage(w, r, http.StatusOK, done)
}
