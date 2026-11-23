package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/sms"
	"github.com/orchard-suite/dispatch/internal/store"
)

// UseSMS answers the texts people send to c's number.
func (s *Server) UseSMS(c *sms.Client) { s.sms = c }

// apiSMS is Event Grid's door for the SMS number: a subscription's
// validation, texts people send, and delivery reports. Event Grid carries
// an issued token as a static Authorization header, so the token check
// stands as on every route.
func (s *Server) apiSMS(w http.ResponseWriter, r *http.Request, by caller) {
	events, err := sms.ParseGrid(http.MaxBytesReader(w, r.Body, sms.MaxBody))
	if err != nil {
		s.logRequest(r.Context(), slog.LevelWarn, "sms events refused", r, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, e := range events {
		switch e.EventType {
		case sms.Validation:
			code, err := sms.ValidationCode(e)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			s.logRequest(r.Context(), slog.LevelInfo, "sms subscription validated", r, "by", string(by))
			writeJSON(w, http.StatusOK, map[string]string{"validationResponse": code})
			return
		case sms.Received:
			var in sms.Inbound
			if err := json.Unmarshal(e.Data, &in); err != nil {
				s.logRequest(r.Context(), slog.LevelWarn, "sms text not read", r, "error", err)
				continue
			}
			s.smsReply(r, in)
		case sms.Delivery:
			var rep sms.Report
			if err := json.Unmarshal(e.Data, &rep); err != nil {
				s.logRequest(r.Context(), slog.LevelWarn, "sms report not read", r, "error", err)
				continue
			}
			id := sms.MessageOf(rep.Tag)
			if id == 0 {
				s.logRequest(r.Context(), slog.LevelInfo, "sms report for no message of ours", r)
				continue
			}
			words := rep.Status
			if rep.StatusDetails != "" {
				words += ": " + rep.StatusDetails
			}
			if err := s.store.Deliver(r.Context(), id, rep.Delivered(), words, time.Now()); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.logRequest(r.Context(), slog.LevelError, "sms report not kept", r, "message", id, "error", err)
				continue
			}
			s.logRequest(r.Context(), slog.LevelInfo, "sms report kept", r, "message", id, "delivered", rep.Delivered())
			s.metrics.Receipt(r.Context(), "sms", map[bool]string{true: "delivered", false: "undelivered"}[rep.Delivered()])
		default:
			s.logRequest(r.Context(), slog.LevelInfo, "event grid event ignored", r, "type", e.EventType)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// smsReply acts on a text someone sent: ACK takes the page that last
// texted them, ESC tells its next step now. Only a number on People is
// heard, and the answer goes back to it.
func (s *Server) smsReply(r *http.Request, in sms.Inbound) {
	ctx := r.Context()
	p, err := s.store.PersonByPhone(ctx, in.From)
	if errors.Is(err, store.ErrNotFound) {
		s.logRequest(ctx, slog.LevelWarn, "sms text from a number nobody has", r)
		s.metrics.Reply(ctx, "sms", "refused")
		return
	}
	if err != nil {
		s.logRequest(ctx, slog.LevelError, "sms sender not read", r, "error", err)
		return
	}
	box := s.desk.Notices()
	reply := sms.ReadReply(in.Message)
	page, err := s.store.ActivePageOf(ctx, p.Email, notice.SMS)
	answer := ""
	switch {
	case reply == sms.ReplyUnknown:
		answer = "Reply ACK to take the page that last texted you, or ESC to pass it to the next step."
	case errors.Is(err, store.ErrNotFound) || box == nil:
		answer = "Nothing is waiting on you."
	case err != nil:
		s.logRequest(ctx, slog.LevelError, "sms page not read", r, "error", err)
		return
	case reply == sms.ReplyAcknowledge:
		if _, err := box.Acknowledge(ctx, page.ID, p.Email); err != nil {
			s.logRequest(ctx, slog.LevelError, "sms acknowledge failed", r, "page", page.ID, "error", err)
			return
		}
		answer = "You have it. Nobody else in the chain is told."
	case reply == sms.ReplyEscalate:
		if _, err := box.Escalate(ctx, page.ID, p.Email); err != nil {
			s.logRequest(ctx, slog.LevelError, "sms escalate failed", r, "page", page.ID, "error", err)
			return
		}
		answer = "Passed on: the next step is told now."
	}
	s.logRequest(ctx, slog.LevelInfo, "sms text answered", r, "reply", string(reply), "page", page.ID)
	s.metrics.Reply(ctx, "sms", replyOutcome(reply))
	if s.sms == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		if _, err := s.sms.Send(ctx, p.Phone, answer, ""); err != nil {
			s.log.WarnContext(ctx, "sms answer not sent", "error", err)
		}
	}()
}

// replyOutcome is a reply's word for the replies counter.
func replyOutcome(r sms.Reply) string {
	switch r {
	case sms.ReplyAcknowledge:
		return "acknowledged"
	case sms.ReplyEscalate:
		return "escalated"
	}
	return "unknown"
}
