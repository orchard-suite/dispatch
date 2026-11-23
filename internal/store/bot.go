package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const botSchema = `
CREATE TABLE IF NOT EXISTS bot_teams (
  team_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  service_url TEXT NOT NULL,
  added_at TEXT NOT NULL
);
`

// BotTeam is a team the Teams bot was added to, and where the bot service
// takes its messages for it.
type BotTeam struct {
	ID         string
	Name       string
	TenantID   string
	ServiceURL string
	AddedAt    time.Time
}

// SaveBotTeam keeps a team the bot is in, or updates its name and service URL.
func (s *Store) SaveBotTeam(ctx context.Context, t BotTeam) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO bot_teams (team_id, name, tenant_id, service_url, added_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (team_id) DO UPDATE SET name = CASE WHEN excluded.name = '' THEN bot_teams.name ELSE excluded.name END, tenant_id = excluded.tenant_id, service_url = excluded.service_url`,
		t.ID, t.Name, t.TenantID, t.ServiceURL, fmtTime(t.AddedAt))
	return err
}

// RemoveBotTeam forgets a team the bot was taken out of.
func (s *Store) RemoveBotTeam(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bot_teams WHERE team_id = ?`, id)
	return err
}

// BotTeams lists the teams the bot is in, by name.
func (s *Store) BotTeams(ctx context.Context) ([]BotTeam, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT team_id, name, tenant_id, service_url, added_at FROM bot_teams ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BotTeam
	for rows.Next() {
		t, err := scanBotTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// BotTeam reads one team the bot is in.
func (s *Store) BotTeam(ctx context.Context, id string) (BotTeam, error) {
	t, err := scanBotTeam(s.db.QueryRowContext(ctx, `SELECT team_id, name, tenant_id, service_url, added_at FROM bot_teams WHERE team_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func scanBotTeam(r interface{ Scan(...any) error }) (BotTeam, error) {
	var t BotTeam
	var at string
	if err := r.Scan(&t.ID, &t.Name, &t.TenantID, &t.ServiceURL, &at); err != nil {
		return t, err
	}
	t.AddedAt = parseTime(at)
	return t, nil
}
