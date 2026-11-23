// Package github opens an issue for an action item and reads whether it
// is closed, with a token that may only touch issues.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/entra"
)

// API is GitHub's REST API.
const API = "https://api.github.com"

// Client holds a token that may read and write issues. The token is a
// secret and never appears in an error.
type Client struct {
	token  string
	api    string
	client *http.Client
}

func New(token string) *Client {
	return &Client{token: strings.TrimSpace(token), api: API, client: telemetry.Client(20 * time.Second)}
}

// UseAPI points the client at another API, for a test's own server.
func (c *Client) UseAPI(url string, h *http.Client) { c.api, c.client = strings.TrimRight(url, "/"), h }

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidRepo says whether s is owner/repo.
func ValidRepo(s string) bool { return repoRe.MatchString(s) }

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := c.client.Do(req)
	if err != nil {
		return entra.Unwrap(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("GitHub answered %d: %s", res.StatusCode, entra.FirstLine(e.Message))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Open opens an issue and answers its web URL.
func (c *Client) Open(ctx context.Context, repo, title, body string) (string, error) {
	if !ValidRepo(repo) {
		return "", errors.New("a repository is owner/repo")
	}
	var got struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.call(ctx, http.MethodPost, "/repos/"+repo+"/issues", map[string]any{"title": title, "body": body}, &got)
	return got.HTMLURL, err
}

var issueRe = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/issues/(\d+)$`)

// Closed says whether the issue at a web URL is closed.
func (c *Client) Closed(ctx context.Context, url string) (bool, error) {
	m := issueRe.FindStringSubmatch(url)
	if m == nil {
		return false, errors.New("not an issue's URL")
	}
	var got struct {
		State string `json:"state"`
	}
	if err := c.call(ctx, http.MethodGet, "/repos/"+m[1]+"/issues/"+m[2], nil, &got); err != nil {
		return false, err
	}
	return got.State == "closed", nil
}
