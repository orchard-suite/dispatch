package server

import (
	"fmt"
	"net/http"
	"time"

	kit "github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// decideAdmin posts an admin's change to how notices are routed to the
// annals: who changed which person, group, chain, schedule, channel,
// subscription, token, or setting.
func (s *Server) decideAdmin(r *http.Request, verb, kind, id, title string, parties ...kit.Party) {
	by := door.From(r.Context()).Email
	now := time.Now()
	s.desk.Annals().Decide(r.Context(), annals.Decision{
		Verb: verb, Kind: kind, ID: id, Title: title, By: incident.Person(by),
		Key: fmt.Sprintf("%s:%s:%s:%d", kind, id, verb, now.UnixNano()), At: now,
		Parties: parties,
	})
}

// sendable says which media this desk can send.
func (s *Server) sendable() map[notice.Medium]bool {
	out := map[notice.Medium]bool{}
	box := s.desk.Notices()
	for _, m := range notice.Media {
		out[m] = box != nil && box.Sends(m)
	}
	return out
}
