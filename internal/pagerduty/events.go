// Package pagerduty reads PagerDuty's Events API v2, so anything that pages
// PagerDuty can page dispatch by changing only the URL. An event becomes
// an alert the desk folds in like Alertmanager's: its dedup key is the
// alert's fingerprint.
package pagerduty

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
)

// MaxBody is PagerDuty's own limit on an event.
const MaxBody = 512 << 10

// Event is one Events API v2 event.
type Event struct {
	RoutingKey  string  `json:"routing_key"`
	EventAction string  `json:"event_action"`
	DedupKey    string  `json:"dedup_key"`
	Payload     Payload `json:"payload"`
	Client      string  `json:"client"`
	ClientURL   string  `json:"client_url"`
	Links       []struct {
		Href string `json:"href"`
		Text string `json:"text"`
	} `json:"links"`
}

// Payload is what a trigger says about the alert.
type Payload struct {
	Summary       string         `json:"summary"`
	Source        string         `json:"source"`
	Severity      string         `json:"severity"`
	Timestamp     string         `json:"timestamp"`
	Component     string         `json:"component"`
	Group         string         `json:"group"`
	Class         string         `json:"class"`
	CustomDetails map[string]any `json:"custom_details"`
}

// Parse reads and checks an event the way PagerDuty does: a routing key,
// a known action, a dedup key for acknowledge and resolve, and for a
// trigger a summary, a source, and a known severity. A trigger without a
// dedup key gets one from its summary and source.
func Parse(r io.Reader) (Event, error) {
	var e Event
	if err := json.NewDecoder(io.LimitReader(r, MaxBody)).Decode(&e); err != nil {
		return e, errors.New("the body is not an Events API v2 event")
	}
	switch {
	case e.RoutingKey == "":
		return e, errors.New("routing_key is required")
	case e.EventAction != "trigger" && e.EventAction != "acknowledge" && e.EventAction != "resolve":
		return e, errors.New("event_action must be trigger, acknowledge, or resolve")
	case len(e.DedupKey) > 255:
		return e, errors.New("dedup_key must be at most 255 characters")
	case e.EventAction != "trigger" && e.DedupKey == "":
		return e, errors.New("dedup_key is required to acknowledge or resolve")
	}
	if e.EventAction == "trigger" {
		p := e.Payload
		switch {
		case strings.TrimSpace(p.Summary) == "" || len(p.Summary) > 1024:
			return e, errors.New("payload.summary is required, at most 1024 characters")
		case strings.TrimSpace(p.Source) == "":
			return e, errors.New("payload.source is required")
		case p.Severity != "critical" && p.Severity != "error" && p.Severity != "warning" && p.Severity != "info":
			return e, errors.New("payload.severity must be critical, error, warning, or info")
		}
		if e.DedupKey == "" {
			sum := sha256.Sum256([]byte(p.Source + "\x00" + p.Summary))
			e.DedupKey = hex.EncodeToString(sum[:16])
		}
	}
	return e, nil
}

// Severity is the desk's severity for PagerDuty's: error pages like
// critical, as PagerDuty's high urgency does.
func Severity(s string) string {
	switch s {
	case "critical", "error":
		return "critical"
	case "info":
		return "info"
	}
	return "warning"
}

// Alert is a trigger as the desk's alert; a resolve carries no payload, so
// the desk resolves the alert its dedup key already names. Its name is the
// event's class, else its component, else "pagerduty"; its service the
// group, else the component, else the source.
func (e Event) Alert(now time.Time) alertmanager.Alert {
	p := e.Payload
	name := firstOf(p.Class, p.Component, "pagerduty")
	labels := map[string]string{
		"alertname": name,
		"service":   firstOf(p.Group, p.Component, p.Source),
		"severity":  Severity(p.Severity),
		"source":    p.Source,
	}
	for k, v := range map[string]string{"component": p.Component, "group": p.Group, "class": p.Class, "pd_severity": p.Severity} {
		if v != "" {
			labels[k] = v
		}
	}
	for k, v := range p.CustomDetails {
		if len(labels) >= 30 {
			break
		}
		if s, ok := v.(string); ok && len(s) <= 200 {
			if _, set := labels[k]; !set {
				labels[k] = s
			}
		}
	}
	starts := now
	if t, err := time.Parse(time.RFC3339, p.Timestamp); err == nil {
		starts = t
	}
	a := alertmanager.Alert{
		Status:      "firing",
		Labels:      labels,
		Annotations: map[string]string{"summary": p.Summary},
		StartsAt:    starts,
		Fingerprint: e.DedupKey,
	}
	if strings.HasPrefix(e.ClientURL, "https://") || strings.HasPrefix(e.ClientURL, "http://") {
		a.GeneratorURL = e.ClientURL
	}
	if e.EventAction == "resolve" {
		a.Status, a.EndsAt = "resolved", now
	}
	return a
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Answer is PagerDuty's answer to an event.
type Answer struct {
	Status   string   `json:"status"`
	Message  string   `json:"message"`
	DedupKey string   `json:"dedup_key,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

// Accepted is the answer to an event taken.
func Accepted(dedup string) Answer {
	return Answer{Status: "success", Message: "Event processed", DedupKey: dedup}
}

// Invalid is the answer to an event refused.
func Invalid(err error) Answer {
	return Answer{Status: "invalid event", Message: "Event object is invalid", Errors: []string{fmt.Sprint(err)}}
}
