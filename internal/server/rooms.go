package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/entra"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// TeamsRooms opens a channel for an incident in one team the bot is in,
// through Microsoft Graph as the bot's own Entra app (which needs Graph's
// Channel.Create application permission), and posts there as the bot.
type TeamsRooms struct {
	team     string
	graph    *entra.Signer
	graphURL string
	client   *http.Client
	send     *botSender
}

// TeamsRooms makes the rooms for the team; the bot must be set up and in
// the team.
func (s *Server) TeamsRooms(team string, graph *entra.Signer) (*TeamsRooms, error) {
	if s.bot == nil || team == "" {
		return nil, errors.New("incident rooms need the Teams bot and INCIDENT_TEAM_ID")
	}
	return &TeamsRooms{team: team, graph: graph, graphURL: "https://graph.microsoft.com", client: telemetry.Client(20 * time.Second), send: s.BotSender()}, nil
}

// UseGraph points the rooms at another Graph, for a test's own server.
func (t *TeamsRooms) UseGraph(u string, c *http.Client) {
	t.graphURL, t.client = strings.TrimRight(u, "/"), c
}

func (t *TeamsRooms) Open(ctx context.Context, inc incident.Incident) (string, error) {
	token, err := t.graph.Token(ctx)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("inc-%d %s", inc.ID, inc.Title)
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`~#%&*{}+/\:<>?|'".`, r) {
			return ' '
		}
		return r
	}, name)
	if len(name) > 50 {
		name = strings.TrimSpace(name[:50])
	}
	body, _ := json.Marshal(map[string]any{"displayName": name, "description": fmt.Sprintf("Incident #%d on %s", inc.ID, inc.Service), "membershipType": "standard"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.graphURL+"/v1.0/teams/"+url.PathEscape(t.team)+"/channels", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := t.client.Do(req)
	if err != nil {
		return "", entra.Unwrap(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode == http.StatusUnauthorized {
		t.graph.Forget()
	}
	if res.StatusCode != http.StatusCreated {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return "", fmt.Errorf("Graph answered %d %s: %s", res.StatusCode, e.Error.Code, entra.FirstLine(e.Error.Message))
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.ID == "" {
		return "", errors.New("Graph's answer named no channel")
	}
	return t.team + "/" + got.ID, nil
}

func (t *TeamsRooms) Post(ctx context.Context, room string, e notice.Event) error {
	status, err := t.send.Send(ctx, room, e)
	if err == nil && status >= 300 {
		err = fmt.Errorf("the bot service answered %d", status)
	}
	return err
}
