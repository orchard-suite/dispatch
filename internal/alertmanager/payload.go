// Package alertmanager reads the webhook Alertmanager posts to a receiver.
// The shape is Alertmanager's documented webhook payload, version 4.
package alertmanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Payload is one webhook notification: a group of alerts that share the
// route's group labels, each firing or resolved.
type Payload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is one alert in the group.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Name is the alertname label.
func (a Alert) Name() string { return a.Labels["alertname"] }

// Firing says whether the alert is still firing.
func (a Alert) Firing() bool { return a.Status == "firing" }

// Resolved says whether Alertmanager reports the alert as resolved. An
// alert with an end time in the past counts as resolved even when its
// status says firing, which Alertmanager does not send but a replayed
// fixture might.
func (a Alert) Resolved() bool {
	return a.Status == "resolved" || (!a.EndsAt.IsZero() && !a.StartsAt.IsZero() && a.EndsAt.After(a.StartsAt) && a.EndsAt.Before(time.Now()))
}

// Ended is the end time, nil while firing. Alertmanager sends the zero
// time "0001-01-01T00:00:00Z" for a firing alert.
func (a Alert) Ended() *time.Time {
	if a.Resolved() && !a.EndsAt.IsZero() {
		t := a.EndsAt
		return &t
	}
	return nil
}

// MaxBody is the largest webhook body read. Alertmanager truncates groups
// itself, so a real body is a few kilobytes.
const MaxBody = 4 << 20

var ErrEmpty = errors.New("the payload has no alerts")

// Parse reads and checks a webhook body. Alerts come back sorted by name
// and fingerprint, so a group is handled in one order every time.
func Parse(r io.Reader) (Payload, error) {
	var p Payload
	dec := json.NewDecoder(io.LimitReader(r, MaxBody))
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("the body is not an Alertmanager webhook payload: %w", err)
	}
	if len(p.Alerts) == 0 {
		return p, ErrEmpty
	}
	for i := range p.Alerts {
		a := &p.Alerts[i]
		if a.Labels == nil {
			a.Labels = map[string]string{}
		}
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		if a.Name() == "" {
			return p, fmt.Errorf("alert %d has no alertname label", i)
		}
		if a.Fingerprint == "" {
			a.Fingerprint = fingerprint(a.Labels)
		}
		if a.Status != "firing" && a.Status != "resolved" {
			return p, fmt.Errorf("alert %d has status %q; firing or resolved expected", i, a.Status)
		}
	}
	sort.SliceStable(p.Alerts, func(i, j int) bool {
		if p.Alerts[i].Name() != p.Alerts[j].Name() {
			return p.Alerts[i].Name() < p.Alerts[j].Name()
		}
		return p.Alerts[i].Fingerprint < p.Alerts[j].Fingerprint
	})
	return p, nil
}

// fingerprint stands in for Alertmanager's when a payload lacks one: the
// sorted label pairs joined, which is unique per label set.
func fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + labels[k] + ";")
	}
	return b.String()
}
