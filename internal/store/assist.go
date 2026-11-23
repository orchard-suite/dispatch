package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/orchard-suite/dispatch/internal/assist"
)

// AssistNote is one thing the assistant was asked about an incident: its
// kind, whether it is done, what it wrote, and the evidence it read.
type AssistNote struct {
	ID         int64
	IncidentID int64
	Kind       string
	State      string
	Text       string
	Evidence   []assist.Step
	AskedBy    string
	AskedAt    time.Time
}

// AskAssist keeps a request to the assistant, pending.
func (s *Store) AskAssist(ctx context.Context, incidentID int64, kind, by string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO assist_notes (incident_id, kind, state, asked_by, asked_at) VALUES (?, ?, 'pending', ?, ?)`, incidentID, kind, by, fmtTime(at))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AnswerAssist keeps what the assistant wrote, or why it could not.
func (s *Store) AnswerAssist(ctx context.Context, id int64, state, text string, evidence []assist.Step) error {
	if evidence == nil {
		evidence = []assist.Step{}
	}
	return s.affected(s.db.ExecContext(ctx, `UPDATE assist_notes SET state = ?, text = ?, evidence = ? WHERE id = ?`, state, text, jsonOf(evidence), id))
}

// AssistNotes lists an incident's notes from the assistant, newest first.
func (s *Store) AssistNotes(ctx context.Context, incidentID int64) ([]AssistNote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, incident_id, kind, state, text, evidence, asked_by, asked_at FROM assist_notes WHERE incident_id = ? ORDER BY id DESC LIMIT 20`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistNote
	for rows.Next() {
		var n AssistNote
		var evidence, at string
		if err := rows.Scan(&n.ID, &n.IncidentID, &n.Kind, &n.State, &n.Text, &evidence, &n.AskedBy, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(evidence), &n.Evidence)
		n.AskedAt = parseTime(at)
		out = append(out, n)
	}
	return out, rows.Err()
}
