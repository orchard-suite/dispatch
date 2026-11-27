package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// changeBody is what POST /v1/changes takes. A field it does not name is
// refused, never dropped.
type changeBody struct {
	Service string           `json:"service"`
	Title   string           `json:"title"`
	Kind    string           `json:"kind"`
	URL     string           `json:"url"`
	By      string           `json:"by"`
	At      time.Time        `json:"at"`
	Things  []change.Ref     `json:"things"`
	Run     string           `json:"run"`
	Record  *change.EventRef `json:"record"`
}

// maxChangeBody is the most of a change a post may carry.
const maxChangeBody = 256 << 10

// maxChangeEventsBody is the most of a batch of change events a post may
// carry.
const maxChangeEventsBody = 8 << 20

// strict decodes exactly one JSON value into v, refusing a field v does
// not have.
func strict(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the body holds more than one JSON value")
	}
	return nil
}

func (s *Server) apiAddChange(w http.ResponseWriter, r *http.Request, by caller) {
	var body changeBody
	if err := strict(w, r, maxChangeBody, &body); err != nil {
		s.logRequest(r.Context(), slog.LevelWarn, "change refused", r, "producer", string(by), "error", err)
		writeError(w, http.StatusBadRequest, "the body must be one change: service or things, title, and only the fields the contract names: "+err.Error())
		return
	}
	c, kept, err := s.desk.AddChange(r.Context(), incident.Change{
		Producer: string(by), Service: body.Service, Title: body.Title, Kind: body.Kind, URL: body.URL, By: body.By, At: body.At,
		Things: body.Things, Run: body.Run, Record: body.Record,
	})
	if s.failed(w, r, err, "change") {
		return
	}
	status := http.StatusCreated
	if !kept {
		status = http.StatusOK
	}
	writeJSON(w, status, c)
}

// apiChanges reads changes back by entity, service, and time.
func (s *Server) apiChanges(w http.ResponseWriter, r *http.Request, _ caller) {
	q := r.URL.Query()
	f := store.ChangeFilter{Service: strings.TrimSpace(q.Get("service"))}
	for _, id := range q["entity"] {
		blueprint, key, ok := strings.Cut(id, ":")
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("entity %q is blueprint:key", id))
			return
		}
		f.Entities = append(f.Entities, change.Ref{Blueprint: blueprint, Key: key})
	}
	for name, at := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, name+" is an RFC 3339 time")
				return
			}
			*at = t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit is 1 to %d", store.MaxChanges))
			return
		}
		f.Limit = n
	}
	if f.Limit == 0 {
		f.Limit = 50
	}
	changes, err := s.desk.Changes(r.Context(), f)
	if s.failed(w, r, err, "changes") {
		return
	}
	if changes == nil {
		changes = []incident.Change{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

// refusedEvents is the answer to a batch of change events refused for
// one of them.
type refusedEvents struct {
	Error  string `json:"error"`
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// apiChangeEvents keeps a batch of the kit's change events, all or none.
// Only a token the desk issued for a producer may send them, and each
// event's sender is that token's name.
func (s *Server) apiChangeEvents(w http.ResponseWriter, r *http.Request, by caller) {
	if !issued(r) {
		s.logRequest(r.Context(), slog.LevelWarn, "change events refused: not a producer's token", r)
		writeError(w, http.StatusForbidden, "change events come with a producer's token from Settings, never a personal token or the shared secret")
		return
	}
	var body struct {
		Events []json.RawMessage `json:"events"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChangeEventsBody))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&body)
	}
	if err != nil {
		s.logRequest(r.Context(), slog.LevelWarn, "change events refused: the body", r, "producer", string(by), "error", err)
		writeJSON(w, http.StatusBadRequest, refusedEvents{Error: "the body must be {\"events\": [...]}: " + err.Error(), Index: 0, Reason: "bad_body"})
		return
	}
	kept, err := s.desk.TakeChangeEvents(r.Context(), string(by), body.Events)
	var refused *desk.RefusedEvent
	if errors.As(err, &refused) {
		writeJSON(w, http.StatusBadRequest, refusedEvents{Error: refused.Error(), Index: refused.Index, Reason: refused.Reason})
		return
	}
	if s.failed(w, r, err, "change events") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "change events taken", r, "producer", string(by), "kept", kept.Kept, "repeats", kept.Repeats)
	writeJSON(w, http.StatusOK, kept)
}
