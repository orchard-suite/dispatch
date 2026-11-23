package server

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
)

type openArgs struct {
	Title       string `json:"title"`
	Severity    string `json:"severity" jsonschema:"critical, warning, or info"`
	Service     string `json:"service" jsonschema:"the component it is on"`
	Description string `json:"description,omitempty" jsonschema:"the first note on its timeline"`
	Public      bool   `json:"public,omitempty" jsonschema:"show it on the status page"`
}

type assignArgs struct {
	ID     int64  `json:"id" jsonschema:"the incident's number"`
	Person string `json:"person" jsonschema:"an email, me to take it, or nobody to clear the owner"`
}

type moveArgs struct {
	ID int64  `json:"id" jsonschema:"the incident's number"`
	To string `json:"to" jsonschema:"acknowledged, mitigated, resolved, or open to reopen a resolved one; get_incident lists the moves allowed now"`
}

type noteArgs struct {
	ID     int64  `json:"id" jsonschema:"the incident's number"`
	Text   string `json:"text" jsonschema:"the note, in Markdown"`
	Public bool   `json:"public,omitempty" jsonschema:"a status update: shown on the status page and mailed to its followers"`
}

type severityArgs struct {
	ID       int64  `json:"id" jsonschema:"the incident's number"`
	Severity string `json:"severity" jsonschema:"critical, warning, or info"`
}

type publicArgs struct {
	ID     int64 `json:"id" jsonschema:"the incident's number"`
	Public bool  `json:"public" jsonschema:"true shows it on the status page"`
}

type roleArgs struct {
	ID     int64  `json:"id" jsonschema:"the incident's number"`
	Role   string `json:"role" jsonschema:"commander, comms, or scribe"`
	Person string `json:"person" jsonschema:"an email, or me"`
}

type linkArgs struct {
	ID    int64 `json:"id" jsonschema:"the incident's number"`
	Other int64 `json:"other" jsonschema:"the other incident's number"`
	Merge bool  `json:"merge,omitempty" jsonschema:"merge this incident into the other: its alerts move there and this one resolves"`
}

type postmortemArgs struct {
	ID   int64  `json:"id" jsonschema:"the incident's number"`
	Body string `json:"body" jsonschema:"the whole postmortem in Markdown; it replaces what is there"`
}

type addActionArgs struct {
	ID    int64  `json:"id" jsonschema:"the incident's number"`
	Text  string `json:"text" jsonschema:"what to change"`
	Owner string `json:"owner,omitempty" jsonschema:"an email, or me"`
}

type actionDoneArgs struct {
	ID     int64 `json:"id" jsonschema:"the incident's number"`
	Action int64 `json:"action_id" jsonschema:"the action item's number"`
	Done   bool  `json:"done" jsonschema:"false opens it again"`
}

// person is the email a person argument names: me is the token's person,
// and nobody is no one.
func person(self, named string) string {
	named = strings.TrimSpace(named)
	switch strings.ToLower(named) {
	case "me":
		return self
	case "nobody":
		return ""
	}
	return named
}

// detail answers a change with the incident as it stands after it.
func (s *Server) detail(ctx context.Context, id int64, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return s.incidentDetail(ctx, id)
}

func (s *Server) mcpActs(server *mcp.Server) {
	act(server, "open_incident", "Open an incident by hand as the person the token acts as.",
		func(ctx context.Context, p string, a openArgs) (any, error) {
			inc, err := s.desk.Open(ctx, desk.OpenRequest{Title: a.Title, Severity: incident.Severity(a.Severity), Service: a.Service, Description: a.Description, Public: a.Public},
				incident.SourceAPI, incident.Person(p))
			return s.detail(ctx, inc.ID, err)
		})
	act(server, "assign_incident", "Give an incident an owner, take it yourself with me, or clear the owner with nobody.",
		func(ctx context.Context, p string, a assignArgs) (any, error) {
			_, err := s.desk.Assign(ctx, a.ID, person(p, a.Person), p)
			return s.detail(ctx, a.ID, err)
		})
	act(server, "move_incident", "Move an incident to another state as the person the token acts as. Acknowledging makes them its owner and stops their pages about it. Only a person resolves an incident: resolve only when the user says it is restored.",
		func(ctx context.Context, p string, a moveArgs) (any, error) {
			_, err := s.desk.Change(ctx, a.ID, incident.State(strings.TrimSpace(a.To)), incident.Person(p))
			return s.detail(ctx, a.ID, err)
		})
	act(server, "add_note", "Add a note to an incident's timeline as the person the token acts as. A public note is a status update.",
		func(ctx context.Context, p string, a noteArgs) (any, error) {
			_, err := s.desk.Note(ctx, a.ID, a.Text, a.Public, p)
			return s.detail(ctx, a.ID, err)
		})
	act(server, "set_severity", "Change an incident's severity.",
		func(ctx context.Context, p string, a severityArgs) (any, error) {
			_, err := s.desk.SetSeverity(ctx, a.ID, incident.Severity(strings.TrimSpace(a.Severity)), p)
			return s.detail(ctx, a.ID, err)
		})
	act(server, "set_public", "Show an incident on the status page, or take it off.",
		func(ctx context.Context, p string, a publicArgs) (any, error) {
			_, err := s.desk.SetPublic(ctx, a.ID, a.Public, p)
			return s.detail(ctx, a.ID, err)
		})
	act(server, "set_role", "Give a person a role on an incident: commander, comms, or scribe.",
		func(ctx context.Context, p string, a roleArgs) (any, error) {
			_, err := s.desk.SetRole(ctx, a.ID, strings.TrimSpace(a.Role), person(p, a.Person), p)
			return s.detail(ctx, a.ID, err)
		})
	act(server, "link_incident", "Link an incident to another, or merge it into the other.",
		func(ctx context.Context, p string, a linkArgs) (any, error) {
			if a.Merge {
				_, err := s.desk.Merge(ctx, a.ID, a.Other, incident.Person(p))
				return s.detail(ctx, a.Other, err)
			}
			return s.detail(ctx, a.ID, s.desk.Link(ctx, a.ID, a.Other, p))
		})
	act(server, "save_postmortem", "Write an incident's postmortem. The body replaces the whole postmortem; start from get_incident's postmortem or its template.",
		func(ctx context.Context, p string, a postmortemArgs) (any, error) {
			if strings.TrimSpace(a.Body) == "" {
				return nil, errors.New("a postmortem needs a body")
			}
			return s.detail(ctx, a.ID, s.desk.SavePostmortem(ctx, a.ID, a.Body, p))
		})
	act(server, "add_action_item", "Add an action item to an incident. It opens an issue when the desk tracks them.",
		func(ctx context.Context, p string, a addActionArgs) (any, error) {
			return s.detail(ctx, a.ID, s.desk.AddAction(ctx, a.ID, a.Text, person(p, a.Owner), p))
		})
	act(server, "set_action_item_done", "Tick an action item done, or open it again.",
		func(ctx context.Context, p string, a actionDoneArgs) (any, error) {
			return s.detail(ctx, a.ID, s.desk.SetActionDone(ctx, a.ID, a.Action, a.Done, p))
		})
	act(server, "acknowledge_page", "Acknowledge a page as the person the token acts as; nobody further in its chain is told.",
		func(ctx context.Context, p string, a pageArgs) (any, error) {
			box := s.desk.Notices()
			if box == nil {
				return nil, errors.New("this desk sends no notices")
			}
			ended, err := box.Acknowledge(ctx, a.Page, p)
			return map[string]bool{"acknowledged": ended}, err
		})
}
