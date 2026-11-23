package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// apiBeat records a heartbeat's check-in. ?interval=5m makes a heartbeat
// it does not know yet.
func (s *Server) apiBeat(w http.ResponseWriter, r *http.Request, by caller) {
	var interval time.Duration
	if v := r.URL.Query().Get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "interval is a duration, such as 5m or 1h")
			return
		}
		interval = d
	}
	h, err := s.desk.Beat(r.Context(), r.PathValue("name"), interval, string(by))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such heartbeat; send ?interval= to make it")
		return
	}
	if s.failed(w, r, err, "heartbeat") {
		return
	}
	s.logRequest(r.Context(), slog.LevelDebug, "heartbeat", r, "heartbeat", h.Name, "by", string(by))
	writeJSON(w, http.StatusOK, h)
}

// apiAddChange keeps a deploy or another change to a service.
func (s *Server) apiAddChange(w http.ResponseWriter, r *http.Request, by caller) {
	var c incident.Change
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be JSON with service and title")
		return
	}
	c.ID, c.Producer = 0, string(by)
	c, err := s.desk.AddChange(r.Context(), c)
	if s.failed(w, r, err, "change") {
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// formSnooze has the desk page the person again about an acknowledged
// incident after the minutes they chose.
func (s *Server) formSnooze(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "snooze", func(id int64) error {
		minutes, _ := strconv.Atoi(r.FormValue("minutes"))
		return s.desk.Snooze(r.Context(), id, minutes, door.From(r.Context()).Email)
	})
}

// formEscalateIncident tells the next step of the incident's pages now.
func (s *Server) formEscalateIncident(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "escalate", func(id int64) error {
		n, err := s.desk.EscalateIncident(r.Context(), id, door.From(r.Context()).Email)
		if err == nil {
			_, err = s.desk.Note(r.Context(), id, fmt.Sprintf("%s told the next step of %d pages now", door.From(r.Context()).Email, n), false, door.From(r.Context()).Email)
		}
		return err
	})
}

func (s *Server) formRole(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "role", func(id int64) error {
		_, err := s.desk.SetRole(r.Context(), id, r.FormValue("role"), r.FormValue("person"), door.From(r.Context()).Email)
		return err
	})
}

// formLink links this incident to another, or merges it into the other.
func (s *Server) formLink(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "link", func(id int64) error {
		other, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(r.FormValue("other")), "#"), 10, 64)
		if err != nil {
			return fmt.Errorf("%w: name the other incident by its number", desk.ErrInput)
		}
		by := door.From(r.Context()).Email
		if r.FormValue("kind") == "merge" {
			_, err = s.desk.Merge(r.Context(), id, other, incident.Person(by))
			return err
		}
		return s.desk.Link(r.Context(), id, other, by)
	})
}

func (s *Server) formRoom(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "room", func(id int64) error {
		_, err := s.desk.OpenRoom(r.Context(), id)
		return err
	})
}

// apiRole gives a person a role on an incident. Body: {"role","person"}.
func (s *Server) apiRole(w http.ResponseWriter, r *http.Request, by caller) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Role, Person string }
	if err != nil || decodeJSON(w, r, &in) != nil {
		writeError(w, http.StatusBadRequest, "the body must be JSON with role and person")
		return
	}
	inc, err := s.desk.SetRole(r.Context(), id, in.Role, in.Person, string(by))
	if s.failed(w, r, err, "role") {
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// apiMerge merges an incident into another. Body: {"into", "actor"}.
func (s *Server) apiMerge(w http.ResponseWriter, r *http.Request, by caller) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Into  int64  `json:"into"`
		Actor string `json:"actor"`
	}
	if err != nil || decodeJSON(w, r, &in) != nil {
		writeError(w, http.StatusBadRequest, "the body must be JSON with into")
		return
	}
	actor := incident.System(string(by))
	actor.For = strings.TrimSpace(in.Actor)
	inc, err := s.desk.Merge(r.Context(), id, in.Into, actor)
	if s.failed(w, r, err, "merge") {
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// formAssist asks the assistant about the incident; its answer shows on the
// page when it is done.
func (s *Server) formAssist(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "assist", func(id int64) error {
		_, err := s.desk.Ask(r.Context(), id, r.FormValue("kind"), door.From(r.Context()).Email)
		return err
	})
}
