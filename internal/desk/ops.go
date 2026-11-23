package desk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// HeartbeatAlert is the alert name a missed heartbeat raises.
const HeartbeatAlert = "HeartbeatMissed"

// Beat records a heartbeat's check-in. A heartbeat that had been late
// resolves its alert. An unknown name is made when interval is given,
// for the producer that sent it.
func (d *Desk) Beat(ctx context.Context, name string, interval time.Duration, producer string) (incident.Heartbeat, error) {
	now := d.Now()
	h, err := d.Store.Heartbeat(ctx, name)
	if errors.Is(err, store.ErrNotFound) && interval > 0 {
		h = incident.Heartbeat{Name: name, Interval: interval, Severity: incident.Critical, CreatedBy: producer, CreatedAt: now}
		if err := h.Validate(); err != nil {
			return h, fmt.Errorf("%w: %w", ErrInput, err)
		}
		if err := d.Store.SaveHeartbeat(ctx, h); err != nil {
			return h, err
		}
		d.log.InfoContext(ctx, "heartbeat made by its first check-in", "heartbeat", name, "producer", producer)
	} else if err != nil {
		return h, err
	}
	late, err := d.Store.Beat(ctx, name, now)
	if err != nil {
		return h, err
	}
	d.metrics.Heartbeat(ctx, "beat")
	if late {
		d.metrics.Heartbeat(ctx, "back")
		d.log.InfoContext(ctx, "heartbeat back", "heartbeat", name)
		if _, err := d.IngestAlerts(ctx, []alertmanager.Alert{heartbeatAlert(h, "resolved", now)}, Machine); err != nil {
			return h, err
		}
	}
	return d.Store.Heartbeat(ctx, name)
}

// SaveHeartbeat adds a heartbeat or changes one.
func (d *Desk) SaveHeartbeat(ctx context.Context, h incident.Heartbeat) error {
	if err := h.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInput, err)
	}
	if incident.ParseSeverity(string(h.Severity)) != h.Severity {
		return fmt.Errorf("%w: a heartbeat's severity is critical, warning, or info", ErrInput)
	}
	h.CreatedAt = d.Now()
	return d.Store.SaveHeartbeat(ctx, h)
}

func heartbeatAlert(h incident.Heartbeat, status string, at time.Time) alertmanager.Alert {
	since := "it was made"
	if h.LastAt != nil {
		since = h.LastAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	a := alertmanager.Alert{
		Status:      status,
		Labels:      map[string]string{"alertname": HeartbeatAlert, "heartbeat": h.Name, "service": h.ServiceName(), "severity": string(h.Severity)},
		Annotations: map[string]string{"summary": h.Name + " has not checked in since " + since},
		StartsAt:    h.Due(),
		Fingerprint: "heartbeat:" + h.Name,
	}
	if status == "resolved" {
		a.EndsAt = at
	}
	return a
}

// CheckHeartbeats raises an alert for every heartbeat whose check-in is
// overdue, once each until it checks in again.
func (d *Desk) CheckHeartbeats(ctx context.Context) {
	beats, err := d.Store.Heartbeats(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "heartbeats not read", "error", err)
		return
	}
	now := d.Now()
	for _, h := range beats {
		if h.Late || now.Before(h.Due()) {
			continue
		}
		marked, err := d.Store.MarkLate(ctx, h.Name)
		if err != nil || !marked {
			continue
		}
		d.log.WarnContext(ctx, "heartbeat late", "heartbeat", h.Name, "due", h.Due())
		d.metrics.Heartbeat(ctx, "late")
		if _, err := d.IngestAlerts(ctx, []alertmanager.Alert{heartbeatAlert(h, "firing", now)}, Machine); err != nil {
			d.log.ErrorContext(ctx, "late heartbeat not raised", "heartbeat", h.Name, "error", err)
		}
	}
}

// Watch checks the heartbeats, the schedules, and the snoozes every half
// minute until ctx ends.
func (d *Desk) Watch(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var lastSync time.Time
	for {
		d.CheckHeartbeats(ctx)
		d.WatchSchedules(ctx)
		d.Remind(ctx)
		if time.Since(lastSync) > 10*time.Minute {
			d.SyncIssues(ctx)
			d.CheckBudgets(ctx)
			d.ResolveQuiet(ctx)
			lastSync = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AddWindow quiets a service for a while.
func (d *Desk) AddWindow(ctx context.Context, w incident.Window) (incident.Window, error) {
	if err := w.Validate(); err != nil {
		return w, fmt.Errorf("%w: %w", ErrInput, err)
	}
	w.CreatedAt = d.Now()
	return d.Store.AddWindow(ctx, w)
}

// Hushed says whether a maintenance window quiets what an event is about,
// by its service label. The outbox asks it of every event.
func (d *Desk) Hushed(ctx context.Context, e notice.Event) bool {
	service := e.Labels["service"]
	if service == "" {
		return false
	}
	in, err := d.Store.InWindow(ctx, service, d.Now())
	if err != nil {
		d.log.ErrorContext(ctx, "windows not read", "error", err)
		return false
	}
	return in
}

// AddChange keeps a change to a service and sends it to the channels as a
// notice, change.<kind>.
func (d *Desk) AddChange(ctx context.Context, c incident.Change) (incident.Change, error) {
	c.Service, c.Title, c.Kind = strings.TrimSpace(c.Service), strings.TrimSpace(c.Title), strings.TrimSpace(c.Kind)
	if c.Kind == "" {
		c.Kind = "deploy"
	}
	switch {
	case c.Service == "" || c.Title == "" || len(c.Title) > 300:
		return c, fmt.Errorf("%w: a change needs a service and a title of at most 300 characters", ErrInput)
	case !incident.ValidChangeKind(c.Kind):
		return c, fmt.Errorf("%w: a change's kind is a lowercase word, such as deploy", ErrInput)
	case c.URL != "" && !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://"):
		return c, fmt.Errorf("%w: a change's url is an absolute http or https URL", ErrInput)
	}
	if c.At.IsZero() {
		c.At = d.Now()
	}
	c, err := d.Store.AddChange(ctx, c)
	if err != nil {
		return c, err
	}
	d.log.InfoContext(ctx, "change kept", "change", c.ID, "component", c.Service, "kind", c.Kind, "producer", c.Producer)
	if d.outbox != nil {
		e := notice.Event{Producer: c.Producer, Kind: "change." + c.Kind, Key: fmt.Sprintf("change:%d", c.ID), Title: c.Title, Weight: notice.WeightNotice, At: c.At,
			Facts: []notice.Fact{{Name: "Service", Value: c.Service}}, Labels: map[string]string{"service": c.Service}}
		if c.By != "" {
			e.Facts = append(e.Facts, notice.Fact{Name: "By", Value: c.By})
		}
		if c.URL != "" {
			e.Links = []notice.Link{{Title: "The change", URL: c.URL}}
		}
		ctx = context.WithoutCancel(ctx)
		go func() {
			if _, err := d.outbox.Accept(ctx, e); err != nil {
				d.log.ErrorContext(ctx, "change notice not taken", "change", c.ID, "error", err)
			}
		}()
	}
	return c, nil
}

// ChangesNear lists the changes to an incident's service from two hours
// before it opened until it resolved, or now.
func (d *Desk) ChangesNear(ctx context.Context, inc incident.Incident) ([]incident.Change, error) {
	until := d.Now()
	if inc.ResolvedAt != nil {
		until = *inc.ResolvedAt
	}
	return d.Store.Changes(ctx, inc.Service, inc.OpenedAt.Add(-2*time.Hour), until.Add(time.Second))
}

// AcknowledgeAtSource notes on its incident that an alert was acknowledged
// where it came from; a person still owns the incident. An alert that
// paged nobody acknowledges its notice's thread instead.
func (d *Desk) AcknowledgeAtSource(ctx context.Context, fingerprint, producer string) (int64, error) {
	now := d.Now()
	var id int64
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		inc, err := d.Store.FindByAlert(ctx, tx, fingerprint)
		if err != nil {
			return err
		}
		id = inc.ID
		_, err = d.event(ctx, tx, inc.ID, now, incident.EventNote, producer, "Acknowledged at its source by "+producer)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		if d.outbox != nil {
			_, err = d.outbox.Accept(ctx, notice.Event{Producer: producer, Kind: "alert.acknowledged", DedupKey: fingerprint, Move: notice.Acknowledge})
		}
		return 0, err
	}
	return id, err
}

// ResolveAtSource marks an alert resolved where it came from, by its
// fingerprint alone: the incident that holds it gets it resolved on its
// timeline, and a person still resolves the incident. An alert that paged
// nobody closes its notice's thread.
func (d *Desk) ResolveAtSource(ctx context.Context, fingerprint, producer string) (IngestResult, error) {
	inc, err := d.Store.FindByAlert(ctx, d.Store.DB(), fingerprint)
	if errors.Is(err, store.ErrNotFound) {
		if d.outbox != nil {
			_, err = d.outbox.Accept(ctx, notice.Event{Producer: producer, Kind: "alert.resolved", DedupKey: fingerprint, Move: notice.Resolve})
		}
		return IngestResult{Opened: []int64{}, Updated: []int64{}, Reopened: []int64{}, Resolved: []int64{}}, err
	}
	if err != nil {
		return IngestResult{}, err
	}
	stored, err := d.Store.GetAlert(ctx, d.Store.DB(), inc.ID, fingerprint)
	if err != nil {
		return IngestResult{}, err
	}
	return d.IngestAlerts(ctx, []alertmanager.Alert{{
		Status: "resolved", Labels: stored.Labels, Annotations: stored.Annotations,
		StartsAt: stored.StartsAt, EndsAt: d.Now(), Fingerprint: fingerprint,
	}}, producer)
}
