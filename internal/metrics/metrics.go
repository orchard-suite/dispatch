// Package metrics is the desk's own instruments. Their names are a
// contract with the deployment's dashboard and alerts.
package metrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Service is the service name the instruments are registered under.
const Service = "dispatch"

// Stats is what the gauges read at each export.
type Stats struct {
	// OpenBySeverity counts incidents not yet resolved, keyed by severity.
	OpenBySeverity map[string]int
}

// Metrics is the server's own instruments.
type Metrics struct {
	opened        metric.Int64Counter
	webhooks      metric.Int64Counter
	notifications metric.Int64Counter
	orrery        metric.Int64Counter
	annals        metric.Int64Counter
	deliveries    metric.Int64Counter
	messages      metric.Int64Counter
	pages         metric.Int64Counter
	alerts        metric.Int64Counter
	heartbeats    metric.Int64Counter
	replies       metric.Int64Counter
	receipts      metric.Int64Counter
	assist        metric.Int64Counter
	changes       metric.Int64Counter
	changeEvents  metric.Int64Counter
	watches       metric.Int64Counter
	sights        metric.Int64Counter
	observed      metric.Int64Counter
	tta           metric.Float64Histogram
	ttr           metric.Float64Histogram
}

// New registers the instruments. stats is called once per export, never
// per request.
func New(stats func() Stats) (*Metrics, error) {
	m := otel.Meter(Service)
	var err error
	mt := &Metrics{}
	if mt.opened, err = m.Int64Counter("dispatch.incidents.opened", metric.WithDescription("Incidents opened, by severity and service.")); err != nil {
		return nil, err
	}
	if mt.webhooks, err = m.Int64Counter("dispatch.webhook.received", metric.WithDescription("Webhooks received, by source and outcome.")); err != nil {
		return nil, err
	}
	if mt.notifications, err = m.Int64Counter("dispatch.notifications.sent", metric.WithDescription("Outbound webhooks, by outcome.")); err != nil {
		return nil, err
	}
	if mt.orrery, err = m.Int64Counter("dispatch.orrery.pushes", metric.WithDescription("Incidents pushed to Orrery, by outcome.")); err != nil {
		return nil, err
	}
	if mt.annals, err = m.Int64Counter("dispatch.annals.posts", metric.WithDescription("Incident decisions posted to Annals, by outcome.")); err != nil {
		return nil, err
	}
	if mt.deliveries, err = m.Int64Counter("dispatch.deliveries", metric.WithDescription("Attempts to deliver an event to a channel, by outcome: sent, retry, or failed.")); err != nil {
		return nil, err
	}
	if mt.messages, err = m.Int64Counter("dispatch.messages", metric.WithDescription("Attempts to tell one person an event by email or Teams, by outcome: sent, retry, failed, or skipped.")); err != nil {
		return nil, err
	}
	if mt.alerts, err = m.Int64Counter("dispatch.alerts", metric.WithDescription("Alerts taken in, by source and outcome: incident, notice, quieted, or dropped.")); err != nil {
		return nil, err
	}
	if mt.heartbeats, err = m.Int64Counter("dispatch.heartbeats", metric.WithDescription("Heartbeats, by outcome: beat, late, or back.")); err != nil {
		return nil, err
	}
	if mt.replies, err = m.Int64Counter("dispatch.replies", metric.WithDescription("Answers people sent back, by source (sms, discord) and outcome: acknowledged, escalated, unknown, or refused.")); err != nil {
		return nil, err
	}
	if mt.receipts, err = m.Int64Counter("dispatch.receipts", metric.WithDescription("Delivery reports on messages, by source and outcome: delivered or undelivered.")); err != nil {
		return nil, err
	}
	if mt.assist, err = m.Int64Counter("dispatch.assist", metric.WithDescription("Asks of the assistant, by outcome: done or failed.")); err != nil {
		return nil, err
	}
	if mt.changes, err = m.Int64Counter("dispatch.changes", metric.WithDescription("Changes posted, by source (the producer) and outcome: kept or repeat.")); err != nil {
		return nil, err
	}
	if mt.changeEvents, err = m.Int64Counter("dispatch.change_events", metric.WithDescription("Change events taken in, by source (the sender) and outcome: kept, repeat, or refused.")); err != nil {
		return nil, err
	}
	if mt.watches, err = m.Int64Counter("dispatch.watches", metric.WithDescription("Watches on changes, by outcome: started, skipped, observed, missed, or unwatched.")); err != nil {
		return nil, err
	}
	if mt.sights, err = m.Int64Counter("dispatch.watch.sights", metric.WithDescription("What the watch saw of one kind on one subject, by outcome: clean, harm, or blind.")); err != nil {
		return nil, err
	}
	if mt.observed, err = m.Int64Counter("dispatch.watch.sends", metric.WithDescription("Observations handed on, by source (the target: annals or orrery) and outcome: sent or retry.")); err != nil {
		return nil, err
	}
	if mt.pages, err = m.Int64Counter("dispatch.pages", metric.WithDescription("Escalation chain pages, by outcome: started, escalated, acknowledged, or unanswered.")); err != nil {
		return nil, err
	}
	buckets := metric.WithExplicitBucketBoundaries(60, 300, 600, 900, 1800, 3600, 7200, 14400, 28800, 86400, 259200)
	if mt.tta, err = m.Float64Histogram("dispatch.time_to_acknowledge_seconds", metric.WithUnit("s"), buckets, metric.WithDescription("Seconds from an incident opening to a person acknowledging it.")); err != nil {
		return nil, err
	}
	if mt.ttr, err = m.Float64Histogram("dispatch.time_to_restore_seconds", metric.WithUnit("s"), buckets, metric.WithDescription("Seconds from an incident opening to its resolution.")); err != nil {
		return nil, err
	}
	open, err := m.Int64ObservableGauge("dispatch.incidents.open", metric.WithDescription("Incidents not yet resolved, by severity."))
	if err != nil {
		return nil, err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := stats()
		for _, sev := range []string{"critical", "warning", "info"} {
			o.ObserveInt64(open, int64(s.OpenBySeverity[sev]), metric.WithAttributes(attribute.String("severity", sev)))
		}
		return nil
	}, open)
	if err != nil {
		return nil, err
	}
	return mt, nil
}

func (m *Metrics) Opened(ctx context.Context, severity, service string) {
	m.opened.Add(ctx, 1, metric.WithAttributes(attribute.String("severity", severity), attribute.String("service", service)))
}

func (m *Metrics) Webhook(ctx context.Context, source, outcome string) {
	m.webhooks.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Notification(ctx context.Context, outcome string) {
	m.notifications.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) OrreryPush(ctx context.Context, outcome string) {
	m.orrery.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) AnnalsPost(ctx context.Context, outcome string) {
	m.annals.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Alert(ctx context.Context, source, outcome string) {
	m.alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Heartbeat(ctx context.Context, outcome string) {
	m.heartbeats.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Reply(ctx context.Context, source, outcome string) {
	m.replies.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Receipt(ctx context.Context, source, outcome string) {
	m.receipts.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Assist(ctx context.Context, outcome string) {
	m.assist.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Change(ctx context.Context, source, outcome string) {
	m.changes.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) ChangeEvent(ctx context.Context, source, outcome string) {
	m.changeEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Watch(ctx context.Context, outcome string) {
	m.watches.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Sight(ctx context.Context, outcome string) {
	m.sights.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) ObservedSend(ctx context.Context, source, outcome string) {
	m.observed.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Delivery(ctx context.Context, outcome string) {
	m.deliveries.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Message(ctx context.Context, outcome string) {
	m.messages.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Page(ctx context.Context, outcome string) {
	m.pages.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Acknowledged(ctx context.Context, severity, service string, seconds float64) {
	m.tta.Record(ctx, seconds, metric.WithAttributes(attribute.String("severity", severity), attribute.String("service", service)))
}

func (m *Metrics) Restored(ctx context.Context, severity, service string, seconds float64) {
	m.ttr.Record(ctx, seconds, metric.WithAttributes(attribute.String("severity", severity), attribute.String("service", service)))
}
