package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// callerKey holds, in a request's context, the token's name.
type callerKey struct{}

// mcpInstructions is what an agent is told when it connects.
const mcpInstructions = `dispatch is the incident desk. An incident moves open, acknowledged, mitigated, resolved; reopening a resolved one is the only move back. Its owner holds it until it is restored.
Start with board: who you act as, what you own, what nobody owns, your open action items, and the postmortems you owe. get_incident gives one incident in full, with the moves it allows.
Tools that change an incident act as the person a personal token was made for on Me, and need such a token. Every change lands on the incident's timeline under that person's name, exactly as if they made it on the desk.`

// apiMCP is the desk as an MCP server for agents, behind the same token
// check as every route: an agent reads the board, incidents, pages, and
// schedules, sends events as its token, and, with a personal token, works
// incidents as that person.
func (s *Server) apiMCP() tokenHandler {
	server := mcp.NewServer(&mcp.Implementation{Name: "dispatch", Version: s.cfg.Version}, &mcp.ServerOptions{Instructions: mcpInstructions})
	s.mcpReads(server)
	s.mcpActs(server)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return func(w http.ResponseWriter, r *http.Request, by caller) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, by)))
	}
}

// who is the token and the person it acts as, from a tool call's context.
func who(ctx context.Context) (caller, string) {
	by, _ := ctx.Value(callerKey{}).(caller)
	person, _ := ctx.Value(personKey{}).(string)
	return by, person
}

var errNeedsPerson = errors.New("this needs a personal token, made on Me, so it acts as a person")

// read adds a tool any token may call. It changes nothing.
func read[A any](server *mcp.Server, name, desc string, fn func(context.Context, A) (any, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, a A) (*mcp.CallToolResult, any, error) {
			v, err := fn(ctx, a)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(v)
		})
}

// act adds a tool that changes something as the person a personal token
// acts as, and refuses any other token.
func act[A any](server *mcp.Server, name, desc string, fn func(ctx context.Context, person string, a A) (any, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *mcp.CallToolRequest, a A) (*mcp.CallToolResult, any, error) {
			_, person := who(ctx)
			if person == "" {
				return nil, nil, errNeedsPerson
			}
			v, err := fn(ctx, person, a)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(v)
		})
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil, nil
}

// unresolved is every state an incident is still burning in.
var unresolved = []incident.State{incident.Open, incident.Acknowledged, incident.Mitigated}

type listArgs struct {
	State    string `json:"state,omitempty" jsonschema:"states separated by commas (open, acknowledged, mitigated, resolved), or all; every unresolved state when left out"`
	Owner    string `json:"owner,omitempty" jsonschema:"an email, me for the person the token acts as, or nobody for incidents without an owner"`
	Service  string `json:"service,omitempty" jsonschema:"keep only this service"`
	Severity string `json:"severity,omitempty" jsonschema:"critical, warning, or info"`
	Query    string `json:"q,omitempty" jsonschema:"words in the title, service, or owner, or an incident's number"`
	Limit    int    `json:"limit,omitempty" jsonschema:"at most this many, 100 when left out, up to 500"`
}

type idArgs struct {
	ID int64 `json:"id" jsonschema:"the incident's number"`
}

type ownerArgs struct {
	Owner string `json:"owner,omitempty" jsonschema:"an email, or me; every open action item when left out"`
}

type windowArgs struct {
	Window string `json:"window,omitempty" jsonschema:"24h, 7d, 30d, or 90d; 30d when left out"`
}

type pageArgs struct {
	Page int64 `json:"page" jsonschema:"the page's number"`
}

type eventArgs struct {
	Kind   string   `json:"kind" jsonschema:"dotted lowercase words, such as agent.finished"`
	Title  string   `json:"title"`
	Text   string   `json:"text,omitempty"`
	Weight string   `json:"weight,omitempty" jsonschema:"notice, notify, or page"`
	To     []string `json:"to,omitempty" jsonschema:"targets such as person:a@example.com, group:slug, chain:slug, schedule:slug"`
}

// me is the person an owner argument names: me is the token's person.
func me(ctx context.Context, owner string) (string, error) {
	owner = strings.TrimSpace(owner)
	if !strings.EqualFold(owner, "me") {
		return owner, nil
	}
	if _, person := who(ctx); person != "" {
		return person, nil
	}
	return "", errNeedsPerson
}

// onCall is the schedules a person is on call on now, with when it ends.
type onCall struct {
	Schedule string    `json:"schedule"`
	Name     string    `json:"name"`
	Person   string    `json:"person"`
	Until    time.Time `json:"until"`
}

func (s *Server) onCallNow(ctx context.Context) ([]onCall, error) {
	now := time.Now()
	schedules, err := s.store.Schedules(ctx, now)
	if err != nil {
		return nil, err
	}
	out := []onCall{}
	for _, sc := range schedules {
		out = append(out, onCall{Schedule: sc.Slug, Name: sc.Name, Person: sc.OnCall(now), Until: sc.Until(now)})
	}
	return out, nil
}

// board is the desk at a glance for one person.
type board struct {
	Token string `json:"token"`
	// Person is who the token acts as; empty for a token that acts as nobody.
	Person  string   `json:"person,omitempty"`
	DeskURL string   `json:"desk_url"`
	OnCall  []onCall `json:"on_call"`
	// OpenBySeverity counts every unresolved incident.
	OpenBySeverity map[string]int `json:"open_by_severity"`
	// Mine is the unresolved incidents the person owns.
	Mine []incident.Incident `json:"mine"`
	// Unowned is the unresolved incidents nobody owns: the ones to take.
	Unowned []incident.Incident `json:"unowned"`
	// Others is the unresolved incidents someone else owns.
	Others []incident.Incident `json:"others"`
	// Settling is the unresolved incidents whose alerts have all stopped:
	// each closes itself at its closes_at unless an alert fires again, so
	// none is work for a person. Mine, Unowned, and Others leave them out.
	Settling []incident.Incident `json:"settling"`
	// Actions is the person's action items not done.
	Actions []actionItem `json:"my_action_items"`
	// Postmortems is the incidents the person owns, resolved in the last
	// thirty days, with no postmortem written.
	Postmortems []incident.Incident `json:"postmortems_due"`
}

// actionItem is an action item with its incident's title.
type actionItem struct {
	incident.ActionItem
	Incident string `json:"incident_title"`
}

func (s *Server) withTitles(ctx context.Context, items []incident.ActionItem) ([]actionItem, error) {
	out := []actionItem{}
	titles := map[int64]string{}
	for _, a := range items {
		title, ok := titles[a.IncidentID]
		if !ok {
			inc, err := s.store.Get(ctx, s.store.DB(), a.IncidentID)
			if err != nil {
				return nil, err
			}
			title, titles[a.IncidentID] = inc.Title, inc.Title
		}
		out = append(out, actionItem{ActionItem: a, Incident: title})
	}
	return out, nil
}

func (s *Server) board(ctx context.Context) (board, error) {
	by, person := who(ctx)
	b := board{Token: string(by), Person: person, DeskURL: s.cfg.PublicURL, Mine: []incident.Incident{}, Unowned: []incident.Incident{},
		Others: []incident.Incident{}, Settling: []incident.Incident{}, Actions: []actionItem{}, Postmortems: []incident.Incident{}}
	var err error
	if b.OnCall, err = s.onCallNow(ctx); err != nil {
		return b, err
	}
	if b.OpenBySeverity, err = s.store.OpenBySeverity(ctx); err != nil {
		return b, err
	}
	burning, err := s.store.List(ctx, store.Filter{States: unresolved, Limit: 500})
	if err != nil {
		return b, err
	}
	for _, inc := range burning {
		switch {
		case inc.Settling():
			b.Settling = append(b.Settling, inc)
		case inc.Owner == "":
			b.Unowned = append(b.Unowned, inc)
		case person != "" && strings.EqualFold(inc.Owner, person):
			b.Mine = append(b.Mine, inc)
		default:
			b.Others = append(b.Others, inc)
		}
	}
	if person == "" {
		return b, nil
	}
	items, err := s.store.OpenActionItems(ctx, person)
	if err != nil {
		return b, err
	}
	if b.Actions, err = s.withTitles(ctx, items); err != nil {
		return b, err
	}
	due, err := s.store.List(ctx, store.Filter{States: []incident.State{incident.Resolved}, Owner: person, NoPostmortem: true,
		Since: time.Now().Add(-30 * 24 * time.Hour), Limit: 50})
	if err != nil {
		return b, err
	}
	if due != nil {
		b.Postmortems = due
	}
	return b, nil
}

// incidentDetail is one incident with everything a person working it
// reads on its page.
type incidentDetail struct {
	incidentView
	URL string `json:"url"`
	// Moves is the states it may move to now.
	Moves []incident.State  `json:"moves"`
	Roles map[string]string `json:"roles"`
	Links []incident.Link   `json:"links"`
	// Changes is the deploys and other changes to its service near it.
	Changes []incident.Change `json:"changes"`
	// About is the catalog's word on its workload: runbook, docs, owner,
	// repository.
	About map[string]string `json:"about,omitempty"`
	// UpdateTemplate starts a status update, while it is unresolved.
	UpdateTemplate string `json:"status_update_template,omitempty"`
	// PostmortemTemplate starts a postmortem, while none is written.
	PostmortemTemplate string `json:"postmortem_template,omitempty"`
}

func (s *Server) incidentDetail(ctx context.Context, id int64) (incidentDetail, error) {
	v, err := s.view(ctx, id)
	if err != nil {
		return incidentDetail{}, err
	}
	d := incidentDetail{incidentView: v, URL: fmt.Sprintf("%s/incidents/%d", s.cfg.PublicURL, id), Moves: []incident.State{},
		About: s.desk.About(ctx, v.Incident)}
	for _, st := range incident.States {
		if incident.Transition(v.State, st) == nil {
			d.Moves = append(d.Moves, st)
		}
	}
	if d.Roles, err = s.store.Roles(ctx, id); err != nil {
		return d, err
	}
	if d.Links, err = s.store.Links(ctx, id); err != nil {
		return d, err
	}
	if d.Changes, err = s.desk.ChangesNear(ctx, v.Incident); err != nil {
		return d, err
	}
	if d.Links == nil {
		d.Links = []incident.Link{}
	}
	if d.Changes == nil {
		d.Changes = []incident.Change{}
	}
	if v.State != incident.Resolved {
		d.UpdateTemplate = s.desk.UpdateTemplate(ctx, v.Incident)
	}
	if v.Postmortem == nil {
		d.PostmortemTemplate = incident.PostmortemTemplate
	}
	return d, nil
}

func (s *Server) mcpReads(server *mcp.Server) {
	read(server, "board", "The desk at a glance for the person the token acts as: who they are, who is on call, open incidents by severity, the unresolved incidents they own, the ones nobody owns, the ones others own, the ones settling (every alert stopped; each closes itself at its closes_at, so none needs working), their open action items, and the postmortems they owe. Call it first.",
		func(ctx context.Context, _ struct{}) (any, error) { return s.board(ctx) })
	read(server, "list_incidents", "List incidents, unresolved ones first by severity and age: number, title, severity, state, service, owner, firing alerts, last update.",
		func(ctx context.Context, a listArgs) (any, error) {
			f := store.Filter{States: unresolved, Service: strings.TrimSpace(a.Service), Severity: incident.Severity(a.Severity), Search: strings.TrimSpace(a.Query), Limit: 100}
			switch st := strings.TrimSpace(a.State); st {
			case "":
			case "all":
				f.States = nil
			default:
				f.States = nil
				for _, one := range strings.Split(st, ",") {
					f.States = append(f.States, incident.State(strings.TrimSpace(one)))
				}
			}
			if strings.EqualFold(strings.TrimSpace(a.Owner), "nobody") {
				f.Unowned = true
			} else {
				owner, err := me(ctx, a.Owner)
				if err != nil {
					return nil, err
				}
				f.Owner = owner
			}
			if a.Limit > 0 {
				f.Limit = min(a.Limit, 500)
			}
			list, err := s.store.List(ctx, f)
			if list == nil {
				list = []incident.Incident{}
			}
			return list, err
		})
	read(server, "get_incident", "One incident in full: its alerts, timeline, roles, linked incidents, changes to its service near it, the catalog's runbook and owner, the moves it allows now, its postmortem and action items, and templates to start a status update or a postmortem.",
		func(ctx context.Context, a idArgs) (any, error) { return s.incidentDetail(ctx, a.ID) })
	read(server, "list_action_items", "Action items not done yet, on every incident, oldest first, each with its incident's number and title.",
		func(ctx context.Context, a ownerArgs) (any, error) {
			owner, err := me(ctx, a.Owner)
			if err != nil {
				return nil, err
			}
			items, err := s.store.OpenActionItems(ctx, owner)
			if err != nil {
				return nil, err
			}
			return s.withTitles(ctx, items)
		})
	read(server, "who_is_on_call", "Who each schedule has on call now, and until when.",
		func(ctx context.Context, _ struct{}) (any, error) { return s.onCallNow(ctx) })
	read(server, "outcomes", "The outcomes report over a window: incidents, MTTA, MTTR, the restore-time trend, and pages, in total, per service, and per person.",
		func(ctx context.Context, a windowArgs) (any, error) { return s.desk.Outcomes(ctx, a.Window) })
	read(server, "my_list", "What reached the person the token acts as, newest first, with each event's weight.",
		func(ctx context.Context, _ struct{}) (any, error) {
			_, person := who(ctx)
			if person == "" {
				return nil, errNeedsPerson
			}
			return s.store.Inbox(ctx, person, 30)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "send_event", Description: "Send an event as the token, to the channels that subscribe to it and to the people, groups, chains, and schedules in to."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a eventArgs) (*mcp.CallToolResult, any, error) {
			by, _ := who(ctx)
			box := s.desk.Notices()
			if box == nil {
				return nil, nil, errors.New("this desk sends no notices")
			}
			e := notice.Event{Producer: string(by), Kind: a.Kind, Title: a.Title, Text: a.Text, Weight: notice.Weight(a.Weight), Key: fmt.Sprintf("mcp:%d", time.Now().UnixNano())}
			for _, t := range a.To {
				target, err := notice.ParseTarget(t)
				if err != nil {
					return nil, nil, err
				}
				e.To = append(e.To, target)
			}
			got, err := box.Accept(ctx, e)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(got)
		})
}
