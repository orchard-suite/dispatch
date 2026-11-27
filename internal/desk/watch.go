package desk

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/config"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/signals"
	"github.com/orchard-suite/dispatch/internal/store"
)

// The two alerts the watch raises, and how long an end waits for its
// statement before it is told as a change with no watch.
const (
	ChangeMissedAlert    = "ChangeMissed"
	ChangeUnwatchedAlert = "ChangeUnwatched"
	strayGrace           = 10 * time.Minute
	// leanGrace is how long past its window a watch waits for the catalog
	// when it cannot read what leans on a subject.
	leanGrace = 30 * time.Minute
)

// The targets an observation is handed on to.
const (
	SendAnnals = "annals"
	SendOrrery = "orrery"
)

// UseSignals gives the watch its signal stores; without them every kind
// is blind.
func (d *Desk) UseSignals(o *signals.Opener) { d.signals = o }

// UseVersion names the desk's version, which its observations carry.
func (d *Desk) UseVersion(v string) { d.version = v }

// maybeWatch starts the watch on a change once it is both stated and
// ended, and ran: from when it ended until the longest window of the
// disruption kinds it states.
func (d *Desk) maybeWatch(ctx context.Context, tx *sql.Tx, id int64, kinds *change.Kinds, now time.Time) error {
	c, err := d.Store.Change(ctx, tx, id)
	if err != nil {
		return err
	}
	if c.EndedAt == nil || c.Digest == "" {
		return nil
	}
	if c.Outcome == change.OutcomeRefused {
		d.metrics.Watch(ctx, "skipped")
		d.log.InfoContext(ctx, "watch not started: the change never ran", "change", id, "outcome", c.Outcome)
		return nil
	}
	window := longestWindow(c.Effects, kinds)
	if window == 0 {
		d.metrics.Watch(ctx, "skipped")
		d.log.InfoContext(ctx, "watch not started: the change states no effect of a watched kind", "change", id, "effects", len(c.Effects))
		return nil
	}
	started, err := d.Store.StartWatch(ctx, tx, id, *c.EndedAt, c.EndedAt.Add(window), now)
	if err != nil {
		return err
	}
	if started {
		d.metrics.Watch(ctx, "started")
		d.log.InfoContext(ctx, "watch started", "change", id, "outcome", c.Outcome, "from", *c.EndedAt, "window", window.String())
	}
	return nil
}

// watched is a kind's window, when the kind is a disruption one with a
// watch; zero otherwise.
func watched(kinds *change.Kinds, name string) time.Duration {
	k, ok := kinds.Get(name)
	if !ok || k.Family != change.FamilyDisruption {
		return 0
	}
	return time.Duration(k.Watch)
}

func longestWindow(effects []change.Effect, kinds *change.Kinds) time.Duration {
	var longest time.Duration
	for _, e := range effects {
		longest = max(longest, watched(kinds, e.Kind))
	}
	return longest
}

// WatchChanges closes every watch whose window has closed, tells of every
// end whose statement never came, and hands on what the watch saw.
func (d *Desk) WatchChanges(ctx context.Context) {
	now := d.Now()
	due, err := d.Store.DueWatches(ctx, now)
	if err != nil {
		d.log.ErrorContext(ctx, "watches not read", "error", err)
	}
	for _, id := range due {
		if err := d.closeWatch(ctx, id); err != nil {
			d.log.ErrorContext(ctx, "watch not closed", "change", id, "error", err)
		}
	}
	strays, err := d.Store.StrayEnds(ctx, now.Add(-strayGrace))
	if err != nil {
		d.log.ErrorContext(ctx, "ends with no statement not read", "error", err)
	}
	for _, e := range strays {
		d.metrics.Watch(ctx, "unwatched")
		d.log.WarnContext(ctx, "change ended with no statement: nothing to watch", "sender", e.Sender, "key", e.Key, "of_sender", e.Of.Sender, "of_key", e.Of.Key)
		d.raiseChangeAlert(ctx, ChangeUnwatchedAlert, "change-unwatched:"+e.Of.Sender+"/"+e.Of.Key, "changes", e.Of.Key,
			fmt.Sprintf("A change ended with no watch: %s's change %s ended, and its statement never came, so nothing it did can be watched", e.Of.Sender, e.Of.Key))
		if err := d.Store.MarkTold(ctx, e.Sender, e.Key); err != nil {
			d.log.ErrorContext(ctx, "end with no statement not marked told", "key", e.Key, "error", err)
		}
	}
	d.handOn(ctx)
}

// look is one thing the watch reads one kind on.
type look struct {
	kind    string
	subject change.Ref
	path    string
	window  time.Duration
}

// closeWatch reads what the change did over each kind's window, joins the
// incidents that opened on what it touched, saves the observation to hand
// on, and raises the watch's alerts.
func (d *Desk) closeWatch(ctx context.Context, id int64) error {
	now := d.Now()
	c, err := d.Store.Change(ctx, d.Store.DB(), id)
	if err != nil {
		return err
	}
	if c.Record == nil {
		return fmt.Errorf("change %d is watched with no record", id)
	}
	w, err := d.Store.ChangeWatch(ctx, d.Store.DB(), id)
	if err != nil {
		return err
	}
	st, err := d.Store.Settings(ctx)
	if err != nil {
		return err
	}
	kinds, err := config.KindsOf(st.Kinds)
	if err != nil {
		return err
	}
	var looks []look
	seen := map[look]bool{}
	add := func(l look) {
		key := look{kind: l.kind, subject: l.subject, path: l.path}
		if !seen[key] {
			seen[key] = true
			looks = append(looks, l)
		}
	}
	watchedSet := map[change.Ref]bool{}
	for _, t := range c.Things {
		watchedSet[t] = true
	}
	for _, e := range c.Effects {
		window := watched(kinds, e.Kind)
		if window == 0 {
			continue
		}
		add(look{kind: e.Kind, subject: e.Subject, path: e.Path, window: window})
		watchedSet[e.Subject] = true
		leaners, err := d.orrery.Leaners(ctx, e.Subject, st.Leaning)
		if err != nil {
			if now.Before(w.Due.Add(leanGrace)) {
				d.log.WarnContext(ctx, "watch waits: what leans on a subject cannot be read", "change", id, "subject", e.Subject.Blueprint+":"+e.Subject.Key, "error", err)
				return nil
			}
			d.log.WarnContext(ctx, "watch closes without what leans on a subject: the catalog stayed out of reach", "change", id, "subject", e.Subject.Blueprint+":"+e.Subject.Key, "error", err)
		}
		for _, l := range leaners {
			add(look{kind: e.Kind, subject: l, path: e.Path, window: window})
			watchedSet[l] = true
		}
	}
	params := map[change.Ref]map[string]string{}
	var sights []change.Sight
	for _, l := range looks {
		p, ok := params[l.subject]
		if !ok {
			p = d.paramsOf(ctx, l.subject)
			params[l.subject] = p
		}
		sights = append(sights, d.see(ctx, id, st.Watch, l, p, w.From))
	}
	incidents, err := d.joinIncidents(ctx, id, watchedSet, w.From, w.Due)
	if err != nil {
		return err
	}
	verdict := overall(sights)
	of := *c.Record
	obs := &change.Observed{
		Header:    change.Header{Event: change.EventObserved, Sender: Machine, Key: observedKey(of), At: now.UTC(), Of: &of, Digest: c.Digest},
		Watch:     change.Watcher{Name: Machine, Version: d.versionName()},
		From:      w.From.UTC(),
		To:        w.Due.UTC(),
		Sights:    sights,
		Incidents: incidents,
	}
	if err := obs.Validate(kinds); err != nil {
		return fmt.Errorf("the observation breaks the contract: %w", err)
	}
	raw, err := json.Marshal(obs)
	if err != nil {
		return err
	}
	err = d.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := d.Store.EndWatch(ctx, tx, id, verdict, sights, incidents, now); err != nil {
			return err
		}
		if err := d.Store.InsertChangeEvent(ctx, tx, store.ChangeEvent{Event: obs.Event, Sender: obs.Sender, Key: obs.Key, Of: obs.Of, ChangeID: id, At: obs.At, Body: raw}, now); err != nil {
			return err
		}
		if d.annals.Enabled() {
			if err := d.Store.SaveSend(ctx, tx, id, SendAnnals, raw, now); err != nil {
				return err
			}
		}
		if d.orrery.Pushes() {
			if err := d.Store.SaveSend(ctx, tx, id, SendOrrery, raw, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	d.metrics.Watch(ctx, "observed")
	for _, s := range sights {
		d.metrics.Sight(ctx, s.Verdict)
	}
	d.log.InfoContext(ctx, "watch closed", "change", id, "verdict", verdict, "sights", len(sights), "incidents", len(incidents), "route", c.Route,
		"to_annals", d.annals.Enabled(), "to_orrery", d.orrery.Pushes())
	d.alertOnWatch(ctx, c, sights, incidents)
	return nil
}

// see reads one kind on one subject over its window through every query
// the settings give for its blueprint.
func (d *Desk) see(ctx context.Context, id int64, ws signals.Settings, l look, params map[string]string, from time.Time) change.Sight {
	sight := change.Sight{Kind: l.kind, Subject: l.subject, Path: l.path}
	queries := ws.For(l.subject.Blueprint, l.kind)
	var looks []signals.Look
	for _, q := range queries {
		sight.Signals = append(sight.Signals, q.Signal)
		looks = append(looks, d.read(ctx, ws, q, params, from, from.Add(l.window)))
	}
	sight.Verdict = signals.Judge(looks)
	subject := l.subject.Blueprint + ":" + l.subject.Key
	if len(queries) == 0 {
		d.log.InfoContext(ctx, "watch blind: no query reads this kind for the blueprint", "change", id, "kind", l.kind, "subject", subject)
		return sight
	}
	for _, lk := range looks {
		if lk.Verdict == change.VerdictBlind {
			d.log.InfoContext(ctx, "watch signal blind", "change", id, "kind", l.kind, "subject", subject, "signal", lk.Signal, "why", lk.Why)
		} else {
			d.log.InfoContext(ctx, "watch signal seen", "change", id, "kind", l.kind, "subject", subject, "signal", lk.Signal, "verdict", lk.Verdict)
		}
	}
	return sight
}

// read reads one query and says what it saw.
func (d *Desk) read(ctx context.Context, ws signals.Settings, q signals.Signal, params map[string]string, from, to time.Time) signals.Look {
	blind := func(why string) signals.Look {
		return signals.Look{Signal: q.Signal, Verdict: change.VerdictBlind, Why: why}
	}
	if d.signals == nil {
		return blind("the desk has no signal stores")
	}
	reader, err := d.signals.Open(q.Store, ws.Stores[q.Store])
	if err != nil {
		return blind("no adapter: " + err.Error())
	}
	v, err := reader.Read(ctx, signals.Query{Text: q.Query, Params: params, From: from, To: to})
	switch {
	case errors.Is(err, signals.ErrNoData):
		return blind("no data")
	case errors.Is(err, signals.ErrMissingParam):
		return blind(err.Error())
	case err != nil:
		return blind("the store failed: " + clipText(err.Error(), 300))
	case v > q.Above:
		return signals.Look{Signal: q.Signal, Verdict: change.VerdictHarm, Why: fmt.Sprintf("%g is above %g", v, q.Above)}
	}
	return signals.Look{Signal: q.Signal, Verdict: change.VerdictClean}
}

// paramsOf is a subject's catalog fields as a query's parameters: key,
// title, and blueprint; each field, and each part of an object field as
// field.part; each relation's first target as relation.key and
// relation.title; and the first of each kind that points at it as
// <-blueprint:relation.key and <-blueprint:relation.title. A subject the
// catalog cannot read has its key, title, and blueprint alone.
func (d *Desk) paramsOf(ctx context.Context, ref change.Ref) map[string]string {
	p := map[string]string{"key": ref.Key, "blueprint": ref.Blueprint}
	f, err := d.orrery.Facts(ctx, ref)
	if err != nil {
		d.log.WarnContext(ctx, "watch reads a subject without its catalog fields", "subject", ref.Blueprint+":"+ref.Key, "error", err)
		return p
	}
	p["title"] = f.Title
	for name, raw := range f.Fields {
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := scalar(x); ok {
					p[name+"."+k] = s
				}
			}
		default:
			if s, ok := scalar(t); ok {
				p[name] = s
			}
		}
	}
	for name, refs := range f.Relations {
		if len(refs) > 0 {
			p[name+".key"], p[name+".title"] = refs[0].Key, refs[0].Title
		}
	}
	for at, refs := range f.Incoming {
		bp, rel, ok := strings.Cut(at, ".")
		if ok && len(refs) > 0 {
			p["<-"+bp+":"+rel+".key"], p["<-"+bp+":"+rel+".title"] = refs[0].Key, refs[0].Title
		}
	}
	return p
}

func scalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return fmt.Sprint(t), true
	case bool:
		return fmt.Sprint(t), true
	}
	return "", false
}

// joinIncidents lists the incidents that opened in the window on an entity
// the change touched, felt, or leans on it.
func (d *Desk) joinIncidents(ctx context.Context, id int64, set map[change.Ref]bool, from, to time.Time) ([]change.Incident, error) {
	opened, err := d.Store.List(ctx, store.Filter{Since: from, Limit: 1000})
	if err != nil {
		return nil, err
	}
	var out []change.Incident
	for _, inc := range opened {
		if inc.OpenedAt.After(to) || inc.Workload == "" || !set[orrery.WorkloadRef(inc.Workload)] {
			continue
		}
		d.log.InfoContext(ctx, "incident joined to a change", "change", id, "incident", inc.ID, "workload", inc.Workload)
		out = append(out, change.Incident{System: Machine, ID: fmt.Sprint(inc.ID)})
		if len(out) == change.MaxIncidents {
			break
		}
	}
	return out, nil
}

// overall is the worst the watch saw: harm, then blind, then clean.
func overall(sights []change.Sight) string {
	looks := make([]signals.Look, len(sights))
	for i, s := range sights {
		looks[i] = signals.Look{Verdict: s.Verdict}
	}
	return signals.Judge(looks)
}

// observedKey is the observation's own key: the change's record and
// observed, or its hash when that is too long for a key.
func observedKey(of change.EventRef) string {
	k := of.Sender + ":" + of.Key + ":observed"
	if len(k) <= change.MaxShort {
		return k
	}
	sum := sha256.Sum256([]byte(k))
	return "observed:" + hex.EncodeToString(sum[:])
}

func (d *Desk) versionName() string {
	if d.version == "" {
		return "dev"
	}
	return d.version
}

// alertOnWatch raises a miss, a change routed to no one whose watch saw
// harm or one an incident was joined to, and a change that ended with
// nothing it touched watched, every kind blind.
func (d *Desk) alertOnWatch(ctx context.Context, c incident.Change, sights []change.Sight, incidents []change.Incident) {
	harm, blind := 0, 0
	for _, s := range sights {
		switch s.Verdict {
		case change.VerdictHarm:
			harm++
		case change.VerdictBlind:
			blind++
		}
	}
	name := c.Name()
	switch {
	case len(incidents) > 0:
		d.metrics.Watch(ctx, "missed")
		d.log.WarnContext(ctx, "change missed: an incident was joined to it", "change", c.ID, "incidents", len(incidents), "route", c.Route)
		d.raiseChangeAlert(ctx, ChangeMissedAlert, fmt.Sprintf("change-missed:%d", c.ID), name, fmt.Sprint(c.ID),
			fmt.Sprintf("A change missed harm: %d incidents opened on what change %d touched while it was watched", len(incidents), c.ID))
	case c.Route == change.RouteNoOne && harm > 0:
		d.metrics.Watch(ctx, "missed")
		d.log.WarnContext(ctx, "change missed: it went to no one and the watch saw harm", "change", c.ID, "harm", harm)
		d.raiseChangeAlert(ctx, ChangeMissedAlert, fmt.Sprintf("change-missed:%d", c.ID), name, fmt.Sprint(c.ID),
			fmt.Sprintf("A change missed harm: change %d went to no one, and the watch saw harm in %d of what it touched", c.ID, harm))
	default:
		d.log.InfoContext(ctx, "change not missed", "change", c.ID, "route", c.Route, "harm", harm, "incidents", len(incidents))
	}
	if len(sights) > 0 && blind == len(sights) {
		d.metrics.Watch(ctx, "unwatched")
		d.log.WarnContext(ctx, "change had no watch: every kind on everything it touched was blind", "change", c.ID, "sights", len(sights))
		d.raiseChangeAlert(ctx, ChangeUnwatchedAlert, fmt.Sprintf("change-unwatched:%d", c.ID), name, fmt.Sprint(c.ID),
			fmt.Sprintf("A change ended with no watch: nothing change %d touched could be watched, every kind was blind", c.ID))
	} else {
		d.log.InfoContext(ctx, "change watched", "change", c.ID, "sights", len(sights), "blind", blind)
	}
}

// raiseChangeAlert raises one of the watch's alerts through the desk's own
// alert path, at warning: a notice to the channels that pages nobody.
func (d *Desk) raiseChangeAlert(ctx context.Context, name, fingerprint, service, changeID, summary string) {
	a := alertmanager.Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": name, "service": service, "severity": string(incident.Warning), "change": changeID},
		Annotations: map[string]string{"summary": summary},
		StartsAt:    d.Now(),
		Fingerprint: fingerprint,
	}
	if _, err := d.IngestAlerts(ctx, []alertmanager.Alert{a}, Machine); err != nil {
		d.log.ErrorContext(ctx, "change alert not raised", "alert", name, "change", changeID, "error", err)
	}
}

// handOn hands each saved observation on to its target, and keeps the
// ones not taken to try again later. None is ever dropped.
func (d *Desk) handOn(ctx context.Context) {
	now := d.Now()
	sends, err := d.Store.DueSends(ctx, now, 50)
	if err != nil {
		d.log.ErrorContext(ctx, "saved observations not read", "error", err)
		return
	}
	for _, x := range sends {
		err := d.sendObserved(ctx, x)
		if err == nil {
			d.metrics.ObservedSend(ctx, x.Target, "sent")
			d.log.InfoContext(ctx, "observation taken", "change", x.ChangeID, "target", x.Target, "attempt", x.Attempts+1)
			if err := d.Store.SendTaken(ctx, x.ID, now); err != nil {
				d.log.ErrorContext(ctx, "observation not marked taken", "change", x.ChangeID, "target", x.Target, "error", err)
			}
			continue
		}
		next := now.Add(sendWait(x.Attempts + 1))
		d.metrics.ObservedSend(ctx, x.Target, "retry")
		d.log.WarnContext(ctx, "observation not taken; it is tried again", "change", x.ChangeID, "target", x.Target, "attempt", x.Attempts+1, "next", next, "error", err)
		if err := d.Store.SendAgain(ctx, x.ID, next, clipText(err.Error(), 300)); err != nil {
			d.log.ErrorContext(ctx, "observation's next try not kept", "change", x.ChangeID, "target", x.Target, "error", err)
		}
	}
}

func (d *Desk) sendObserved(ctx context.Context, x store.Send) error {
	ev, err := change.Decode(x.Body)
	if err != nil {
		return err
	}
	obs, ok := ev.(*change.Observed)
	if !ok {
		return fmt.Errorf("a saved observation is a %s", ev.Head().Event)
	}
	switch x.Target {
	case SendAnnals:
		return d.annals.PostNow(ctx, annals.Observed(obs))
	case SendOrrery:
		return d.orrery.ChangeEvents(ctx, []json.RawMessage{x.Body})
	}
	return fmt.Errorf("no target %q", x.Target)
}

// sendWait is how long a send waits before its next try: the notice
// waits, then the longest of them, again and again.
func sendWait(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(notice.Waits) {
		return notice.Waits[len(notice.Waits)-1]
	}
	return notice.Waits[attempt-1]
}
