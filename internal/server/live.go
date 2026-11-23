package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"
)

// livePoll is how often the stream looks for a change; liveBeat is how
// often it says it is still there, so proxies keep the connection.
const (
	livePoll = 2 * time.Second
	liveBeat = 25 * time.Second
)

// liveStream tells an open desk page that what it shows changed, as
// server-sent events. It sends no data a page could not fetch itself: the
// page fetches its own URL again and swaps the parts that moved.
func (s *Server) liveStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mark, err := s.store.Mark(ctx)
	if err != nil {
		s.logRequest(ctx, slog.LevelError, "live stream could not read the mark", r, "error", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	flush := func() bool { return rc.Flush() == nil }
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	if !flush() {
		s.logRequest(ctx, slog.LevelError, "live stream cannot flush", r)
		return
	}
	poll, beat := time.NewTicker(livePoll), time.NewTicker(liveBeat)
	defer poll.Stop()
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": beat\n\n")
			flush()
		case <-poll.C:
			next, err := s.store.Mark(ctx)
			if err != nil || next == mark {
				continue
			}
			mark = next
			fmt.Fprint(w, "event: change\ndata: {}\n\n")
			flush()
		}
	}
}

// deskScript is the desk's one script, served from the binary.
func (s *Server) deskScript(w http.ResponseWriter, r *http.Request) {
	b, err := templateFS.ReadFile("templates/desk.js")
	if err != nil {
		http.Error(w, "missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// shiftAhead is how far the on-call pill looks for a handoff.
const shiftAhead = 60 * 24 * time.Hour

// shiftPill is the rail's on-call pill: whether the person is on call on
// any schedule now, and until when, or who is when they are not.
type shiftPill struct {
	OnCall bool
	// Until is when the person's shift ends, or the other person's.
	Until time.Time
	// Who is the person on call when it is not the viewer.
	Who      string
	Schedule string
	Any      bool
}

func (s *Server) shift(r *http.Request) shiftPill {
	me := strings.ToLower(door.From(r.Context()).Email)
	now := time.Now()
	schedules, err := s.store.Schedules(r.Context(), now)
	if err != nil {
		return shiftPill{}
	}
	var pill shiftPill
	for _, sc := range schedules {
		person := sc.OnCall(now)
		if person == "" {
			continue
		}
		// A shift that runs to the edge of the look-ahead has no handoff in
		// sight, so the pill names none rather than the window's end.
		var until time.Time
		ahead := now.Add(shiftAhead)
		if shifts := sc.Shifts(now, ahead); len(shifts) > 0 && shifts[0].To.Before(ahead) {
			until = shifts[0].To
		}
		if strings.EqualFold(person, me) {
			return shiftPill{OnCall: true, Until: until, Schedule: sc.Name, Any: true}
		}
		if !pill.Any {
			pill = shiftPill{Who: person, Until: until, Schedule: sc.Name, Any: true}
		}
	}
	return pill
}
