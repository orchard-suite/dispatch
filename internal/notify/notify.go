// Package notify sends one generic JSON webhook when an incident opens and
// when its state changes, so the receiver can route it where it likes.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// Message is the body of every notification.
type Message struct {
	// Event is "incident.opened" or "incident.state_changed".
	Event    string            `json:"event"`
	At       time.Time         `json:"at"`
	Incident incident.Incident `json:"incident"`
	// From is the state before a change; empty when opened.
	From incident.State `json:"from,omitempty"`
	// Actor is who made the change.
	Actor string `json:"actor"`
	URL   string `json:"url"`
}

type Webhook struct {
	url     string
	secret  string
	client  *http.Client
	log     *slog.Logger
	outcome func(ctx context.Context, outcome string)
}

// New makes a sender. With no URL it is off and Send does nothing.
func New(url string, log *slog.Logger, outcome func(ctx context.Context, outcome string)) *Webhook {
	return &Webhook{
		url:     url,
		client:  telemetry.Client(10 * time.Second),
		log:     log,
		outcome: outcome,
	}
}

func (w *Webhook) Enabled() bool { return w.url != "" }

// SignWith signs every post with secret, the way a webhook channel's posts
// are signed; empty signs nothing.
func (w *Webhook) SignWith(secret string) { w.secret = secret }

// Send posts the message. It never blocks the caller's request: it runs
// in its own goroutine with its own deadline and logs the outcome.
func (w *Webhook) Send(ctx context.Context, m Message) {
	if !w.Enabled() {
		return
	}
	body, err := json.Marshal(m)
	if err != nil {
		w.log.ErrorContext(ctx, "notification not encoded", "error", err)
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
		if err != nil {
			w.log.ErrorContext(ctx, "notification not built", "error", err)
			w.outcome(ctx, "failed")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "dispatch")
		if w.secret != "" {
			req.Header.Set(notice.SignatureHeader, notice.Sign([]string{w.secret}, time.Now(), body))
		}
		res, err := w.client.Do(req)
		if err != nil {
			w.log.WarnContext(ctx, "notification failed", "event", m.Event, "incident", m.Incident.ID, "error", err)
			w.outcome(ctx, "failed")
			return
		}
		res.Body.Close()
		if res.StatusCode >= 300 {
			w.log.WarnContext(ctx, "notification refused", "event", m.Event, "incident", m.Incident.ID, "status", res.StatusCode)
			w.outcome(ctx, "refused")
			return
		}
		w.log.InfoContext(ctx, "notification sent", "event", m.Event, "incident", m.Incident.ID, "status", res.StatusCode)
		w.outcome(ctx, "sent")
	}()
}
