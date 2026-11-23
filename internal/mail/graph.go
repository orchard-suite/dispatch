package mail

import (
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
	"github.com/orchard-suite/dispatch/internal/notice"
)

// GraphConfig is the Entra app that sends as From and where it signs in.
// The app holds no secret: the workload token is its client assertion.
type GraphConfig struct {
	AppID     string
	TenantID  string
	TokenFile string
	From      string
	// LoginURL and GraphURL are Microsoft's public cloud when empty; tests
	// point them at their own server.
	LoginURL string
	GraphURL string
}

// Graph sends through Microsoft Graph's sendMail as the From mailbox.
type Graph struct {
	cfg    GraphConfig
	sign   *entra.Signer
	client *http.Client
}

func NewGraph(cfg GraphConfig) (*Graph, error) {
	if cfg.AppID == "" || cfg.TenantID == "" || cfg.TokenFile == "" {
		return nil, errors.New("mail: Graph needs MAIL_GRAPH_APP_ID, a tenant, and a federated token file")
	}
	if cfg.GraphURL == "" {
		cfg.GraphURL = "https://graph.microsoft.com"
	}
	sign, err := entra.New(entra.Config{Name: "the mail app", AppID: cfg.AppID, TenantID: cfg.TenantID, TokenFile: cfg.TokenFile,
		Scope: "https://graph.microsoft.com/.default", LoginURL: cfg.LoginURL})
	if err != nil {
		return nil, err
	}
	return &Graph{cfg: cfg, sign: sign, client: telemetry.Client(20 * time.Second)}, nil
}

// UseClient sends with c, for a test against its own TLS server.
func (g *Graph) UseClient(c *http.Client) {
	g.client = c
	g.sign.UseClient(c)
}

func (g *Graph) Kind() string { return "graph" }

// Send posts one letter to sendMail and answers Graph's status. A sign-in
// failure is a 0, so it is tried again.
func (g *Graph) Send(ctx context.Context, to string, l notice.Letter) (int, error) {
	token, err := g.sign.Token(ctx)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"subject":      l.Subject,
			"body":         map[string]string{"contentType": "HTML", "content": l.HTML},
			"toRecipients": []any{map[string]any{"emailAddress": map[string]string{"address": to}}},
		},
		"saveToSentItems": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.cfg.GraphURL, "/")+"/v1.0/users/"+url.PathEscape(g.cfg.From)+"/sendMail", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := g.client.Do(req)
	if err != nil {
		return 0, entra.Unwrap(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		g.sign.Forget()
	}
	if res.StatusCode != http.StatusAccepted {
		var said struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		_ = json.Unmarshal(raw, &said)
		return res.StatusCode, fmt.Errorf("Graph answered %d %s: %s", res.StatusCode, said.Error.Code, entra.FirstLine(said.Error.Message))
	}
	return res.StatusCode, nil
}
