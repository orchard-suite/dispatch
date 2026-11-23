package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/config"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// apiExport answers the desk's routing as the YAML document PUT takes.
func (s *Server) apiExport(w http.ResponseWriter, r *http.Request, _ caller) {
	doc, err := s.desk.Export(r.Context())
	if s.failed(w, r, err, "export") {
		return
	}
	var b bytes.Buffer
	if err := config.Write(&b, doc); err != nil {
		writeError(w, http.StatusInternalServerError, "the document could not be written")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b.Bytes())
}

// apiApply makes the desk's routing what a YAML document says. A personal
// token may not: anyone on the desk can make one, and the document
// changes everyone's routing.
func (s *Server) apiApply(w http.ResponseWriter, r *http.Request, by caller) {
	if p, ok := r.Context().Value(personKey{}).(string); ok && p != "" {
		s.logRequest(r.Context(), slog.LevelWarn, "config refused: a personal token", r)
		writeError(w, http.StatusForbidden, "a personal token cannot change the desk's routing; use a token from Settings")
		return
	}
	doc, err := config.Read(http.MaxBytesReader(w, r.Body, config.MaxBody))
	if err == nil {
		applied, applyErr := s.desk.Apply(r.Context(), doc, string(by))
		if err = applyErr; err == nil {
			now := time.Now()
			s.desk.Annals().Decide(r.Context(), annals.Decision{Verb: "config.apply", Kind: "config", ID: "desk", By: incident.System(string(by)),
				Key: fmt.Sprintf("config:%s:%d", by, now.UnixNano()), At: now})
			s.logRequest(r.Context(), slog.LevelInfo, "config applied", r, "by", string(by))
			writeJSON(w, http.StatusOK, applied)
			return
		}
	}
	if errors.Is(err, notice.ErrInvalid) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.failed(w, r, err, "apply")
}
