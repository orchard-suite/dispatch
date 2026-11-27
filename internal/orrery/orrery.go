// Package orrery talks to the estate's catalog: it resolves the workload a
// namespace links to, and pushes each incident as an `it_incident` entity
// related to that workload, so Orrery shows open incidents on the thing.
package orrery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"
	"github.com/orchard-suite/platformkit/v3/orrery"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// Config is read from the environment. URL is the ingest service's base;
// reads use Token and pushes use PushToken. Either half works without the
// other, and with no read token the push token is used for reads too.
type Config struct {
	URL       string
	Token     string
	PushToken string
	// DeskURL is where an entity links back to, such as https://dispatch.example.org.
	DeskURL string
}

// Scope is the hub scope every incident is written under.
const Scope = "dispatch"

type Client struct {
	read    *orrery.Client
	push    *orrery.Client
	deskURL string
	log     *slog.Logger
	outcome func(ctx context.Context, outcome string)

	mu    sync.Mutex
	cache map[string]cached
	reach map[string]reached
	edges map[string]edged

	offerMu sync.Mutex
	offered bool
}

type cached struct {
	workload string
	at       time.Time
}

const (
	cacheFor = 10 * time.Minute
	// lookupFor bounds a namespace lookup, which runs inside a webhook.
	lookupFor = 20 * time.Second
	pushFor   = 30 * time.Second
)

// New makes the client. A half without a URL or token is off, and so is a
// half whose URL is not absolute, which is logged.
func New(cfg Config, log *slog.Logger, outcome func(ctx context.Context, outcome string)) *Client {
	if cfg.Token == "" {
		cfg.Token = cfg.PushToken
	}
	c := &Client{deskURL: cfg.DeskURL, log: log, outcome: outcome, cache: map[string]cached{}, reach: map[string]reached{}, edges: map[string]edged{}}
	c.read = hub(cfg.URL, cfg.Token, log)
	c.push = hub(cfg.URL, cfg.PushToken, log)
	return c
}

func hub(base, token string, log *slog.Logger) *orrery.Client {
	if base == "" || token == "" {
		return nil
	}
	if u, err := url.Parse(base); err != nil || u.Scheme == "" || u.Host == "" {
		log.Warn("orrery off: the URL is not absolute", "url", base)
		return nil
	}
	return orrery.New(base, token, orrery.WithLogger(log))
}

// Resolves says whether namespaces can be looked up.
func (c *Client) Resolves() bool { return c.read != nil }

// Pushes says whether incidents are sent to the hub.
func (c *Client) Pushes() bool { return c.push != nil }

// Workload finds the workload the cluster namespace links to, "" when the
// hub does not know it or cannot be reached. Answers are cached briefly.
func (c *Client) Workload(ctx context.Context, namespace string) string {
	if !c.Resolves() || namespace == "" {
		return ""
	}
	c.mu.Lock()
	if hit, ok := c.cache[namespace]; ok && time.Since(hit.at) < cacheFor {
		c.mu.Unlock()
		return hit.workload
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	entities, err := c.read.Entities(ctx, orrery.Query{
		Blueprint: "k8s_namespace",
		Filters:   []orrery.Filter{{Field: "name", Op: "eq", Value: namespace}},
	})
	if err != nil {
		c.log.WarnContext(ctx, "orrery namespace lookup failed", "namespace", namespace, "error", err)
		return ""
	}
	workload := ""
	for _, e := range entities {
		if w, ok := e.Related("workload"); ok {
			workload = w.Key
			break
		}
	}
	c.mu.Lock()
	c.cache[namespace] = cached{workload: workload, at: time.Now()}
	c.mu.Unlock()
	c.log.DebugContext(ctx, "orrery namespace resolved", "namespace", namespace, "workload", workload)
	return workload
}

// Push writes the incident to the hub. It runs in the background and
// never fails the caller.
func (c *Client) Push(ctx context.Context, i incident.Incident) {
	if !c.Pushes() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, pushFor)
		defer cancel()
		if !c.offer(ctx) {
			c.outcome(ctx, "no_blueprint")
			return
		}
		e := orrery.Entity{
			Blueprint: "it_incident",
			Key:       Key(i),
			Title:     fmt.Sprintf("#%d %s", i.ID, i.Title),
			Fields:    Fields(i, c.deskURL),
		}
		if i.Workload != "" {
			e.Relations = map[string]any{"workload": i.Workload}
		}
		if err := c.push.Put(ctx, e, orrery.InScope(Scope)); err != nil {
			c.log.WarnContext(ctx, "orrery push failed", "incident", i.ID, "error", err)
			c.outcome(ctx, "failed")
			return
		}
		c.log.InfoContext(ctx, "orrery push done", "incident", i.ID, "workload", i.Workload)
		c.outcome(ctx, "pushed")
	}()
}

// Key is the entity key of an incident.
func Key(i incident.Incident) string { return fmt.Sprintf("dispatch-%d", i.ID) }

// Fields is what the entity carries. Names match the blueprint in
// deploy/orrery-catalog/model/incidents.yaml.
func Fields(i incident.Incident, deskURL string) map[string]any {
	f := map[string]any{
		"severity":   string(i.Severity),
		"state":      string(i.State),
		"phase":      i.Phase(),
		"firing":     i.FiringAlerts > 0,
		"service":    i.Service,
		"owner":      i.Owner,
		"source":     string(i.Source),
		"opened_at":  i.OpenedAt.UTC().Format(time.RFC3339),
		"updated_at": i.UpdatedAt.UTC().Format(time.RFC3339),
		"url":        strings.TrimRight(deskURL, "/") + fmt.Sprintf("/incidents/%d", i.ID),
	}
	if i.AcknowledgedAt != nil {
		f["acknowledged_at"] = i.AcknowledgedAt.UTC().Format(time.RFC3339)
		f["time_to_acknowledge_seconds"] = int(i.AcknowledgedAt.Sub(i.OpenedAt).Seconds())
	}
	if i.MitigatedAt != nil {
		f["mitigated_at"] = i.MitigatedAt.UTC().Format(time.RFC3339)
	}
	if i.ClosesAt != nil {
		f["closes_at"] = i.ClosesAt.UTC().Format(time.RFC3339)
	}
	if i.ResolvedAt != nil {
		f["resolved_at"] = i.ResolvedAt.UTC().Format(time.RFC3339)
		f["time_to_restore_seconds"] = int(i.ResolvedAt.Sub(i.OpenedAt).Seconds())
	}
	return f
}

// Blueprint is offered to the hub before the first push. The hub creates
// it when it has none and extends an existing one with fields it lacks.
var Blueprint = orrery.Blueprint{
	"id":          "it_incident",
	"title":       "Incident",
	"group":       "IT",
	"formerly":    []string{"incident"},
	"icon":        "alert",
	"description": "Record. One incident the dispatch desk holds: an alert group a person owns until it is restored, with its severity, state, owner, and times. Kept ninety days from its opening.",
	"status":      map[string]any{"field": "phase"},
	"lifecycle":   map[string]any{"expire": map[string]any{"after": "90d", "field": "opened_at"}},
	"fields": map[string]any{
		"severity":                    map[string]any{"type": "enum", "values": []string{"critical", "warning", "info"}, "chip": true, "overridable": false},
		"state":                       map[string]any{"type": "enum", "values": []string{"open", "acknowledged", "mitigated", "resolved"}, "chip": true, "overridable": false},
		"phase":                       map[string]any{"type": "enum", "values": []string{"open", "acknowledged", "mitigated", "settling", "resolved"}, "chip": true, "overridable": false},
		"firing":                      map[string]any{"type": "boolean", "title": "Firing", "overridable": false},
		"closes_at":                   map[string]any{"type": "datetime", "title": "Closes itself", "overridable": false},
		"service":                     map[string]any{"type": "string", "overridable": false},
		"owner":                       map[string]any{"type": "string", "overridable": false},
		"source":                      map[string]any{"type": "string", "chip": true, "overridable": false},
		"opened_at":                   map[string]any{"type": "datetime", "title": "Opened", "overridable": false},
		"acknowledged_at":             map[string]any{"type": "datetime", "title": "Acknowledged", "overridable": false},
		"mitigated_at":                map[string]any{"type": "datetime", "title": "Mitigated", "overridable": false},
		"resolved_at":                 map[string]any{"type": "datetime", "title": "Resolved", "overridable": false},
		"updated_at":                  map[string]any{"type": "datetime", "title": "Updated", "overridable": false},
		"time_to_acknowledge_seconds": map[string]any{"type": "number", "title": "Time to acknowledge (s)", "overridable": false},
		"time_to_restore_seconds":     map[string]any{"type": "number", "title": "Time to restore (s)", "overridable": false},
		"url":                         map[string]any{"type": "url", "title": "Desk", "overridable": false},
		"notes":                       map[string]any{"type": "markdown"},
	},
	"relations": map[string]any{
		"workload": map[string]any{"target": "sw_workload", "many": false, "title": "Workload", "inverseTitle": "Incidents"},
	},
}

// offer gives the hub the blueprint once; until the hub takes it, every
// push tries again.
func (c *Client) offer(ctx context.Context) bool {
	c.offerMu.Lock()
	defer c.offerMu.Unlock()
	if c.offered {
		return true
	}
	if _, err := c.push.OfferBlueprints(ctx, []orrery.Blueprint{Blueprint}); err != nil {
		c.log.WarnContext(ctx, "orrery blueprint not offered", "error", err)
		return false
	}
	c.log.InfoContext(ctx, "orrery blueprint offered")
	c.offered = true
	return true
}

// reachHops is how far Reach follows relations: an app service reaches its
// program in one hop, and the program's own parent in two.
const reachHops = 2

type reached struct {
	ids []string
	at  time.Time
}

// Reach lists the ids the entity reaches by following its relations out,
// up to two hops: what a subscription anchored on a program needs to take
// an event about one of the program's app services. Empty when the hub
// cannot be read. Answers are cached briefly.
func (c *Client) Reach(ctx context.Context, ref notice.Ref) []string {
	if !c.Resolves() {
		return nil
	}
	id := ref.ID()
	c.mu.Lock()
	if hit, ok := c.reach[id]; ok && time.Since(hit.at) < cacheFor {
		c.mu.Unlock()
		return hit.ids
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	seen := map[string]bool{id: true}
	var out []string
	edge := []notice.Ref{ref}
	for hop := 0; hop < reachHops && len(edge) > 0; hop++ {
		var next []notice.Ref
		for _, at := range edge {
			var rec orrery.Record
			if err := c.read.Call(ctx, "GET", "/v1/entities/"+url.PathEscape(at.Blueprint)+"/"+url.PathEscape(at.Key), nil, &rec); err != nil {
				c.log.WarnContext(ctx, "orrery reach lookup failed", "entity", at.ID(), "error", err)
				continue
			}
			for _, refs := range rec.Relations {
				for _, r := range refs {
					to := notice.Ref{Blueprint: r.Blueprint, Key: r.Key}
					if seen[to.ID()] {
						continue
					}
					seen[to.ID()] = true
					out = append(out, to.ID())
					next = append(next, to)
				}
			}
		}
		edge = next
	}
	c.mu.Lock()
	c.reach[id] = reached{ids: out, at: time.Now()}
	c.mu.Unlock()
	c.log.DebugContext(ctx, "orrery reach resolved", "entity", id, "reaches", len(out))
	return out
}

// About is what the catalog says of a workload a person working its
// incident wants: its runbook, docs, owner, repository, and URL, each
// only when set.
func (c *Client) About(ctx context.Context, workload string) map[string]string {
	if !c.Resolves() || workload == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	entities, err := c.read.Entities(ctx, orrery.Query{
		Blueprint: "sw_workload",
		Filters:   []orrery.Filter{{Field: "$key", Op: "eq", Value: workload}},
	})
	if err != nil || len(entities) == 0 {
		if err != nil {
			c.log.WarnContext(ctx, "orrery workload lookup failed", "workload", workload, "error", err)
		}
		return nil
	}
	out := map[string]string{}
	for _, field := range []string{"runbook", "docs", "owner", "repo", "url"} {
		var v string
		if raw, ok := entities[0].Fields[field]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
			out[field] = v
		}
	}
	return out
}

// WorkloadRef is the catalog entity of an incident's workload.
func WorkloadRef(workload string) change.Ref {
	return change.Ref{Blueprint: "sw_workload", Key: workload}
}

// edge is one relation between two entities, as the hub's walk answers it.
type edge struct {
	From     change.Ref `json:"from"`
	To       change.Ref `json:"to"`
	Relation string     `json:"relation"`
}

type edged struct {
	edges []edge
	at    time.Time
}

// edgesOf reads every relation into and out of an entity, one hop. An
// entity the hub does not know has none. Answers are cached briefly.
func (c *Client) edgesOf(ctx context.Context, ref change.Ref) ([]edge, error) {
	id := ref.Blueprint + ":" + ref.Key
	c.mu.Lock()
	if hit, ok := c.edges[id]; ok && time.Since(hit.at) < cacheFor {
		c.mu.Unlock()
		return hit.edges, nil
	}
	c.mu.Unlock()
	var walk struct {
		Edges []edge `json:"edges"`
	}
	err := c.read.Call(ctx, "GET", "/v1/entities/"+url.PathEscape(ref.Blueprint)+"/"+url.PathEscape(ref.Key)+"/related?direction=both&depth=1", nil, &walk)
	var api *orrery.APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		err, walk.Edges = nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.edges[id] = edged{edges: walk.Edges, at: time.Now()}
	c.mu.Unlock()
	return walk.Edges, nil
}

// Leaners lists the entities that lean on ref, by the leaning's relations,
// up to its hops, nearest first. It fails when the hub cannot be read.
func (c *Client) Leaners(ctx context.Context, ref change.Ref, l incident.Leaning) ([]change.Ref, error) {
	return c.lean(ctx, ref, l, true)
}

// LeansOn lists the entities ref leans on, by the leaning's relations, up
// to its hops, nearest first. It fails when the hub cannot be read.
func (c *Client) LeansOn(ctx context.Context, ref change.Ref, l incident.Leaning) ([]change.Ref, error) {
	return c.lean(ctx, ref, l, false)
}

func (c *Client) lean(ctx context.Context, ref change.Ref, l incident.Leaning, leaners bool) ([]change.Ref, error) {
	hops := l.Hops()
	if hops == 0 {
		return nil, nil
	}
	if !c.Resolves() {
		return nil, errors.New("orrery: the catalog cannot be read: no ORRERY_URL or token")
	}
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	seen := map[change.Ref]bool{ref: true}
	var out []change.Ref
	edge := []change.Ref{ref}
	for hop := 0; hop < hops && len(edge) > 0; hop++ {
		var next []change.Ref
		for _, at := range edge {
			edges, err := c.edgesOf(ctx, at)
			if err != nil {
				return out, fmt.Errorf("orrery: relations of %s:%s: %w", at.Blueprint, at.Key, err)
			}
			for _, e := range edges {
				forward, back := l.Leans(e.From.Blueprint, e.Relation)
				var to change.Ref
				switch {
				// e.From leans on e.To when forward; e.To leans on e.From when back.
				case leaners && forward && e.To == at, !leaners && back && e.To == at:
					to = e.From
				case leaners && back && e.From == at, !leaners && forward && e.From == at:
					to = e.To
				default:
					continue
				}
				if seen[to] {
					continue
				}
				seen[to] = true
				out = append(out, to)
				next = append(next, to)
			}
		}
		edge = next
	}
	c.log.DebugContext(ctx, "orrery leaning read", "entity", ref.Blueprint+":"+ref.Key, "leaners", leaners, "found", len(out))
	return out, nil
}

// Facts is what the catalog holds of one entity for the watch's queries:
// its fields, the first targets of its relations, and the first entities
// of each kind that point at it, keyed as "blueprint.relation".
type Facts struct {
	Title     string                     `json:"title"`
	Fields    map[string]json.RawMessage `json:"fields"`
	Relations map[string][]orrery.Ref    `json:"relations"`
	Incoming  map[string][]orrery.Ref    `json:"incomingRefs"`
}

// Facts reads one entity. An entity the hub does not know answers
// ErrUnknown.
func (c *Client) Facts(ctx context.Context, ref change.Ref) (Facts, error) {
	var f Facts
	if !c.Resolves() {
		return f, errors.New("orrery: the catalog cannot be read: no ORRERY_URL or token")
	}
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	err := c.read.Call(ctx, "GET", "/v1/entities/"+url.PathEscape(ref.Blueprint)+"/"+url.PathEscape(ref.Key), nil, &f)
	var api *orrery.APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		return f, ErrUnknown
	}
	return f, err
}

// ErrUnknown is an entity the catalog does not hold.
var ErrUnknown = errors.New("orrery: the catalog holds no such entity")

// ChangeEvents posts change events to the hub's ingest, as this desk's
// source. It fails when the hub refuses them or cannot be reached.
func (c *Client) ChangeEvents(ctx context.Context, events []json.RawMessage) error {
	if !c.Pushes() {
		return errors.New("orrery: no ORRERY_PUSH_TOKEN to send change events with")
	}
	ctx, cancel := context.WithTimeout(ctx, pushFor)
	defer cancel()
	return c.push.Call(ctx, "POST", "/v1/ingest/change-events", map[string]any{"events": events}, nil)
}
