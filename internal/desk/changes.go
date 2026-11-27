package desk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/config"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/store"
)

// ChangesBefore is how long before an incident opened a change to what it
// is on is shown beside it.
const ChangesBefore = 2 * time.Hour

// leanNearFor bounds reading what an incident's entity leans on, which
// runs while its page is drawn.
const leanNearFor = 3 * time.Second

// MaxChangeEvents is the most change events one batch carries.
const MaxChangeEvents = 500

// AddChange keeps a change to a service or to catalog entities and sends
// it to the channels as a notice, change.<kind>. A change whose record is
// kept already is answered with the kept one and false, and told nowhere
// again.
func (d *Desk) AddChange(ctx context.Context, c incident.Change) (incident.Change, bool, error) {
	c.Service, c.Title, c.Kind, c.Run = strings.TrimSpace(c.Service), strings.TrimSpace(c.Title), strings.TrimSpace(c.Kind), strings.TrimSpace(c.Run)
	if c.Kind == "" {
		c.Kind = "deploy"
	}
	switch {
	case c.Service == "" && len(c.Things) == 0:
		return c, false, fmt.Errorf("%w: a change names its service, the catalog things it touched, or both", ErrInput)
	case c.Title == "" || len(c.Title) > 300:
		return c, false, fmt.Errorf("%w: a change needs a title of at most 300 characters", ErrInput)
	case !incident.ValidChangeKind(c.Kind):
		return c, false, fmt.Errorf("%w: a change's kind is a lowercase word, such as deploy", ErrInput)
	case c.URL != "" && !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://"):
		return c, false, fmt.Errorf("%w: a change's url is an absolute http or https URL", ErrInput)
	case len(c.Run) > change.MaxShort || strings.ContainsAny(c.Run, "\n\r\x00"):
		return c, false, fmt.Errorf("%w: a change's run is one line of at most %d characters", ErrInput, change.MaxShort)
	case len(c.Things) > change.MaxThings:
		return c, false, fmt.Errorf("%w: a change names at most %d things", ErrInput, change.MaxThings)
	}
	for i, t := range c.Things {
		if err := t.Validate(fmt.Sprintf("things[%d]", i)); err != nil {
			return c, false, fmt.Errorf("%w: %v", ErrInput, err)
		}
	}
	if c.Record != nil {
		if err := c.Record.Validate("record"); err != nil {
			return c, false, fmt.Errorf("%w: %v", ErrInput, err)
		}
	}
	if c.At.IsZero() {
		c.At = d.Now()
	}
	c.Digest, c.Effects, c.Route, c.Outcome, c.EndedAt, c.Events = "", nil, "", "", nil, nil
	c, kept, err := d.Store.AddChange(ctx, c)
	if err != nil {
		return c, false, err
	}
	d.metrics.Change(ctx, c.Producer, outcomeOf(kept))
	if !kept {
		d.log.InfoContext(ctx, "change not kept: its record is kept already", "change", c.ID, "record_sender", c.Record.Sender, "record_key", c.Record.Key, "producer", c.Producer)
		return c, false, nil
	}
	d.log.InfoContext(ctx, "change kept", "change", c.ID, "component", c.Service, "things", len(c.Things), "kind", c.Kind, "producer", c.Producer)
	d.announceChange(ctx, c)
	return c, true, nil
}

func outcomeOf(kept bool) string {
	if kept {
		return "kept"
	}
	return "repeat"
}

// announceChange sends a kept change to the channels as a notice.
func (d *Desk) announceChange(ctx context.Context, c incident.Change) {
	if d.outbox == nil {
		return
	}
	e := notice.Event{Producer: c.Producer, Kind: "change." + c.Kind, Key: fmt.Sprintf("change:%d", c.ID), Title: c.Title, Weight: notice.WeightNotice, At: c.At}
	if c.Service != "" {
		e.Facts = append(e.Facts, notice.Fact{Name: "Service", Value: c.Service})
		e.Labels = map[string]string{"service": c.Service}
	}
	if len(c.Things) > 0 {
		t := c.Things[0]
		e.Entity = &notice.Ref{Blueprint: t.Blueprint, Key: t.Key}
		e.Facts = append(e.Facts, notice.Fact{Name: "Touched", Value: thingsLine(c.Things)})
	}
	if c.By != "" {
		e.Facts = append(e.Facts, notice.Fact{Name: "By", Value: c.By})
	}
	if c.Outcome != "" {
		e.Facts = append(e.Facts, notice.Fact{Name: "Outcome", Value: c.Outcome})
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

// thingsLine names up to five things, and how many more.
func thingsLine(things []change.Ref) string {
	var parts []string
	for i, t := range things {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(things)-5))
			break
		}
		parts = append(parts, t.Blueprint+":"+t.Key)
	}
	return strings.Join(parts, ", ")
}

// ChangesNear lists the changes to an incident's service, and to the
// catalog entity it is on and everything that entity leans on, from two
// hours before it opened until it resolved, or now. When the catalog cannot
// be read, the changes to the service and the entity alone are listed.
func (d *Desk) ChangesNear(ctx context.Context, inc incident.Incident) ([]incident.Change, error) {
	until := d.Now()
	if inc.ResolvedAt != nil {
		until = *inc.ResolvedAt
	}
	f := store.ChangeFilter{Service: inc.Service, Since: inc.OpenedAt.Add(-ChangesBefore), Until: until.Add(time.Second), Limit: 50}
	f.Entities = d.incidentEntities(ctx, inc)
	return d.Store.Changes(ctx, f)
}

// incidentEntities is the catalog entity an incident is on, when it
// resolved to one, and what that entity leans on.
func (d *Desk) incidentEntities(ctx context.Context, inc incident.Incident) []change.Ref {
	if inc.Workload == "" {
		return nil
	}
	on := orrery.WorkloadRef(inc.Workload)
	out := []change.Ref{on}
	st, err := d.Store.Settings(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "settings not read for an incident's changes", "incident", inc.ID, "error", err)
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, leanNearFor)
	defer cancel()
	leans, err := d.orrery.LeansOn(ctx, on, st.Leaning)
	if err != nil {
		d.log.WarnContext(ctx, "an incident's changes read without what it leans on", "incident", inc.ID, "error", err)
	}
	return append(out, leans...)
}

// Changes lists changes by service, entity, and time.
func (d *Desk) Changes(ctx context.Context, f store.ChangeFilter) ([]incident.Change, error) {
	if f.Limit < 0 || f.Limit > store.MaxChanges {
		return nil, fmt.Errorf("%w: limit is 1 to %d", ErrInput, store.MaxChanges)
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return nil, fmt.Errorf("%w: until is after since", ErrInput)
	}
	for i, e := range f.Entities {
		if err := e.Validate(fmt.Sprintf("entity[%d]", i)); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInput, err)
		}
	}
	return d.Store.Changes(ctx, f)
}

// Kinds is the effect kinds the desk knows: the suite's base set and the
// estate's own from the settings.
func (d *Desk) Kinds(ctx context.Context) (*change.Kinds, error) {
	st, err := d.Store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	return config.KindsOf(st.Kinds)
}

// RefusedEvent is a batch of change events refused for one of them: none
// of the batch is kept.
type RefusedEvent struct {
	Index  int
	Reason string
	Detail string
}

func (e *RefusedEvent) Error() string {
	return fmt.Sprintf("event %d refused: %s: %s", e.Index, e.Reason, e.Detail)
}

// Kept says what a batch of change events did.
type Kept struct {
	Kept    int `json:"kept"`
	Repeats int `json:"repeats"`
}

// keptEvent is one event of a batch with what it decoded to.
type keptEvent struct {
	ev     change.Event
	head   *change.Header
	raw    []byte
	repeat bool
}

// TakeChangeEvents keeps a batch of the kit's change events from one
// producer, all or none. Each is decoded strictly, checked against the
// contract and the estate's kinds, and sent by the producer itself: its
// sender is the token's name. An event already kept by its sender and key
// is a repeat and changes nothing. A change.stated event makes a change
// keyed to its things, with its effects; the others attach to it by their
// of, whichever comes first. A change is announced once it ended having
// run, at its end.
func (d *Desk) TakeChangeEvents(ctx context.Context, producer string, raws []json.RawMessage) (Kept, error) {
	var res Kept
	if len(raws) == 0 || len(raws) > MaxChangeEvents {
		return res, &RefusedEvent{Index: 0, Reason: "bad_batch", Detail: fmt.Sprintf("a batch holds 1 to %d events", MaxChangeEvents)}
	}
	kinds, err := d.Kinds(ctx)
	if err != nil {
		return res, err
	}
	batch := make([]keptEvent, len(raws))
	inBatch := map[change.EventRef]int{}
	digests := map[change.EventRef]string{}
	for i, raw := range raws {
		ev, err := change.Decode(raw)
		if err != nil {
			reason := change.Reason(err)
			if reason == "" {
				reason = "bad_event"
			}
			return res, d.refuseEvent(ctx, producer, i, reason, err.Error())
		}
		if err := ev.Validate(kinds); err != nil {
			return res, d.refuseEvent(ctx, producer, i, change.Reason(err), err.Error())
		}
		h := ev.Head()
		self := change.EventRef{Sender: h.Sender, Key: h.Key}
		batch[i] = keptEvent{ev: ev, head: h, raw: raw}
		if _, ok := inBatch[self]; ok {
			batch[i].repeat = true
			continue
		}
		inBatch[self] = i
		had, err := d.Store.HasChangeEvent(ctx, d.Store.DB(), h.Sender, h.Key)
		if err != nil {
			return res, err
		}
		if had {
			batch[i].repeat = true
			continue
		}
		if h.Sender != producer {
			return res, d.refuseEvent(ctx, producer, i, "sender_mismatch", fmt.Sprintf("the token is %q's and the event says it is %q's", producer, h.Sender))
		}
		if h.Event == change.EventStated {
			digests[self] = h.Digest
			continue
		}
		want, ok := digests[*h.Of]
		if !ok {
			if c, err := d.Store.ChangeByRecord(ctx, d.Store.DB(), *h.Of); err == nil && c.Digest != "" {
				want, ok = c.Digest, true
			} else if err != nil && !errors.Is(err, store.ErrNotFound) {
				return res, err
			}
		}
		if ok && want != h.Digest {
			return res, d.refuseEvent(ctx, producer, i, "digest_mismatch", fmt.Sprintf("the change it names has digest %s, the event says %s", want, h.Digest))
		}
	}
	touched := map[int64]bool{}
	now := d.Now()
	err = d.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, k := range batch {
			if k.repeat {
				res.Repeats++
				continue
			}
			id, err := d.keepChangeEvent(ctx, tx, producer, k, kinds, now)
			if err != nil {
				return err
			}
			if id != 0 {
				touched[id] = true
			}
			res.Kept++
		}
		return nil
	})
	if err != nil {
		return Kept{}, err
	}
	for range res.Kept {
		d.metrics.ChangeEvent(ctx, producer, "kept")
	}
	for range res.Repeats {
		d.metrics.ChangeEvent(ctx, producer, "repeat")
	}
	d.log.InfoContext(ctx, "change events kept", "producer", producer, "kept", res.Kept, "repeats", res.Repeats, "changes", len(touched))
	for id := range touched {
		d.announceIfRan(ctx, id)
	}
	return res, nil
}

// announceIfRan sends a change its events made to the channels once it
// ended having run (ok, or failed part way), at its end, and only once. A
// change refused or not yet ended is never announced.
func (d *Desk) announceIfRan(ctx context.Context, id int64) {
	claimed, err := d.Store.ClaimAnnouncement(ctx, id)
	if err != nil {
		d.log.ErrorContext(ctx, "change announcement not claimed", "change", id, "error", err)
		return
	}
	if !claimed {
		d.log.DebugContext(ctx, "change not announced: it has not ended having run, or was announced already", "change", id)
		return
	}
	c, err := d.Store.Change(ctx, d.Store.DB(), id)
	if err != nil {
		d.log.ErrorContext(ctx, "an ended change not read back", "change", id, "error", err)
		return
	}
	d.log.InfoContext(ctx, "change announced: it ended having run", "change", id, "outcome", c.Outcome)
	d.announceChange(ctx, c)
}

func (d *Desk) refuseEvent(ctx context.Context, producer string, i int, reason, detail string) error {
	d.metrics.ChangeEvent(ctx, producer, "refused")
	d.log.WarnContext(ctx, "change events refused", "producer", producer, "index", i, "reason", reason, "error", detail)
	return &RefusedEvent{Index: i, Reason: reason, Detail: detail}
}

// keepChangeEvent writes one event and what it says of its change. It
// answers the change the event is of, once that change is kept.
func (d *Desk) keepChangeEvent(ctx context.Context, tx *sql.Tx, producer string, k keptEvent, kinds *change.Kinds, now time.Time) (int64, error) {
	h := k.head
	row := store.ChangeEvent{Event: h.Event, Sender: h.Sender, Key: h.Key, Of: h.Of, At: h.At, Body: k.raw}
	stated, ok := k.ev.(*change.Stated)
	if !ok {
		c, err := d.Store.ChangeByRecord(ctx, tx, *h.Of)
		switch {
		case errors.Is(err, store.ErrNotFound):
			d.log.InfoContext(ctx, "change event kept before its change", "event", h.Event, "sender", h.Sender, "key", h.Key, "of_sender", h.Of.Sender, "of_key", h.Of.Key)
		case err != nil:
			return 0, err
		default:
			row.ChangeID = c.ID
			if err := d.applyChangeEvent(ctx, tx, c.ID, k.ev); err != nil {
				return 0, err
			}
		}
		if err := d.Store.InsertChangeEvent(ctx, tx, row, now); err != nil {
			return 0, err
		}
		if row.ChangeID != 0 {
			return row.ChangeID, d.maybeWatch(ctx, tx, row.ChangeID, kinds, now)
		}
		return 0, nil
	}
	s := stated.Statement
	self := change.EventRef{Sender: h.Sender, Key: h.Key}
	c, err := d.Store.ChangeByRecord(ctx, tx, self)
	made := false
	switch {
	case errors.Is(err, store.ErrNotFound):
		title := fmt.Sprintf("%s change %s", s.Writer.Name, s.Key)
		if len(title) > 300 {
			title = title[:300]
		}
		id, err := d.Store.InsertChange(ctx, tx, incident.Change{
			Producer: producer, Things: s.Things, Kind: incident.ChangeKindOf(s.Artifact.Kind), Title: title, By: s.Actor.ID, At: h.At,
			Record: &self, Digest: h.Digest, Effects: s.Effects,
		})
		if err != nil {
			return 0, err
		}
		c.ID, made = id, true
	case err != nil:
		return 0, err
	default:
		// A change posted with this record before its events takes the statement.
		if err := d.Store.StateChange(ctx, tx, c.ID, h.Digest, s.Effects, s.Things); err != nil {
			return 0, err
		}
	}
	row.ChangeID = c.ID
	if err := d.Store.InsertChangeEvent(ctx, tx, row, now); err != nil {
		return 0, err
	}
	early, err := d.Store.ChangeEventsOf(ctx, tx, self)
	if err != nil {
		return 0, err
	}
	for _, e := range early {
		if e.ChangeID != 0 {
			continue
		}
		ev, err := change.Decode(e.Body)
		if err != nil {
			return 0, fmt.Errorf("a kept change event no longer decodes: %w", err)
		}
		if err := d.applyChangeEvent(ctx, tx, c.ID, ev); err != nil {
			return 0, err
		}
	}
	if err := d.Store.AttachChangeEvents(ctx, tx, self, c.ID); err != nil {
		return 0, err
	}
	d.log.InfoContext(ctx, "change stated", "change", c.ID, "made", made, "sender", h.Sender, "key", h.Key, "things", len(s.Things), "effects", len(s.Effects), "early_events", len(early))
	return c.ID, d.maybeWatch(ctx, tx, c.ID, kinds, now)
}

// applyChangeEvent keeps on its change what an event after the statement
// says: the route and how it ended.
func (d *Desk) applyChangeEvent(ctx context.Context, tx *sql.Tx, id int64, ev change.Event) error {
	switch e := ev.(type) {
	case *change.Routed:
		return d.Store.RouteChange(ctx, tx, id, e.Route.To)
	case *change.Ended:
		return d.Store.EndChange(ctx, tx, id, e.Outcome, e.At)
	}
	return nil
}
