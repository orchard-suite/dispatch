// Package mail sends one letter to one address, through Microsoft Graph
// signed in with the pod's federated workload token, or through an SMTP
// server. Exactly one is configured. A sender says what the far end
// answered as an HTTP-like status, so the outbox's retry rules hold for
// both: a 4xx means trying again will not help.
package mail

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// Sender sends one letter to one address.
type Sender interface {
	Send(ctx context.Context, to string, l notice.Letter) (int, error)
	// Kind names the transport for the logs: graph or smtp.
	Kind() string
}

// Config is the mail settings from the environment.
type Config struct {
	// From is the mailbox the letters come from.
	From string
	// GraphAppID turns on Graph: the Entra app that holds Mail.Send on From.
	GraphAppID string
	// GraphTenantID and TokenFile are where the app signs in and the
	// federated workload token it signs in with.
	GraphTenantID string
	TokenFile     string
	// SMTPURL turns on SMTP: smtp://user:pass@host:587 (STARTTLS) or
	// smtps://user:pass@host:465. It holds a password and is never logged.
	SMTPURL string
}

// FromEnv reads MAIL_FROM, MAIL_GRAPH_APP_ID, MAIL_GRAPH_TENANT_ID
// (default AZURE_TENANT_ID), MAIL_GRAPH_TOKEN_FILE (default
// AZURE_FEDERATED_TOKEN_FILE), and MAIL_SMTP_URL.
func FromEnv() Config {
	env := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return os.Getenv(fallback)
	}
	return Config{
		From:          os.Getenv("MAIL_FROM"),
		GraphAppID:    os.Getenv("MAIL_GRAPH_APP_ID"),
		GraphTenantID: env("MAIL_GRAPH_TENANT_ID", "AZURE_TENANT_ID"),
		TokenFile:     env("MAIL_GRAPH_TOKEN_FILE", "AZURE_FEDERATED_TOKEN_FILE"),
		SMTPURL:       os.Getenv("MAIL_SMTP_URL"),
	}
}

// New makes the configured sender, or nil when mail is off.
func New(cfg Config) (Sender, error) {
	switch {
	case cfg.GraphAppID == "" && cfg.SMTPURL == "":
		return nil, nil
	case cfg.GraphAppID != "" && cfg.SMTPURL != "":
		return nil, errors.New("mail: set MAIL_GRAPH_APP_ID or MAIL_SMTP_URL, not both")
	case cfg.From == "":
		return nil, errors.New("mail: MAIL_FROM is required")
	}
	if _, err := notice.NormalEmail(cfg.From); err != nil {
		return nil, fmt.Errorf("mail: MAIL_FROM: %w", err)
	}
	if cfg.GraphAppID != "" {
		return NewGraph(GraphConfig{AppID: cfg.GraphAppID, TenantID: cfg.GraphTenantID, TokenFile: cfg.TokenFile, From: cfg.From})
	}
	return NewSMTP(cfg.SMTPURL, cfg.From)
}
