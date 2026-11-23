package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// ErrKindTaken is a kind another producer already declared.
var ErrKindTaken = errors.New("another producer declared this kind")

// DeclareKinds replaces the whole set of kinds one producer declared. A
// kind it kept keeps the choices people made for it; a kind it dropped
// takes its choices with it. A kind another producer holds is refused,
// named, with ErrKindTaken, and nothing changes. It answers how many
// kinds it dropped.
func (s *Store) DeclareKinds(ctx context.Context, producer string, kinds []notice.DeclaredKind, at time.Time) (dropped int, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		keep := map[string]bool{}
		for _, k := range kinds {
			keep[k.Kind] = true
			var holder string
			err := tx.QueryRowContext(ctx, `SELECT producer FROM event_kinds WHERE kind = ?`, k.Kind).Scan(&holder)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && holder != producer {
				return fmt.Errorf("%w: %s", ErrKindTaken, k.Kind)
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT kind FROM event_kinds WHERE producer = ?`, producer)
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var kind string
			if err := rows.Scan(&kind); err != nil {
				rows.Close()
				return err
			}
			if !keep[kind] {
				gone = append(gone, kind)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, kind := range gone {
			if _, err := tx.ExecContext(ctx, `DELETE FROM event_kinds WHERE kind = ?`, kind); err != nil {
				return err
			}
		}
		dropped = len(gone)
		for _, k := range kinds {
			if _, err := tx.ExecContext(ctx, `INSERT INTO event_kinds (kind, producer, title, description, weight, declared_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (kind) DO UPDATE SET title = excluded.title, description = excluded.description, weight = excluded.weight`,
				k.Kind, producer, k.Title, k.Description, string(k.Weight), fmtTime(at)); err != nil {
				return err
			}
		}
		return nil
	})
	return dropped, err
}

// DeclaredKinds lists every declared kind, by producer and then kind.
func (s *Store) DeclaredKinds(ctx context.Context) ([]notice.DeclaredKind, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT producer, kind, title, description, weight FROM event_kinds ORDER BY producer, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.DeclaredKind
	for rows.Next() {
		var k notice.DeclaredKind
		var weight string
		if err := rows.Scan(&k.Producer, &k.Kind, &k.Title, &k.Description, &weight); err != nil {
			return nil, err
		}
		k.Weight = notice.Weight(weight)
		out = append(out, k)
	}
	return out, rows.Err()
}

// Choices are the kinds one person set, by kind.
func (s *Store) Choices(ctx context.Context, email string) (map[string]notice.Choice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, media FROM person_kinds WHERE email = ?`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]notice.Choice{}
	for rows.Next() {
		c, err := scanChoice(rows)
		if err != nil {
			return nil, err
		}
		out[c.Kind] = c
	}
	return out, rows.Err()
}

func scanChoice(r interface{ Scan(...any) error }) (notice.Choice, error) {
	var c notice.Choice
	var media string
	if err := r.Scan(&c.Kind, &media); err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(media), &c.Media)
}

// Choice is what one person set for one kind a producer declared;
// ErrNotFound when they set nothing, or when another producer holds the
// kind.
func (s *Store) Choice(ctx context.Context, email, producer, kind string) (notice.Choice, error) {
	c, err := scanChoice(s.db.QueryRowContext(ctx, `SELECT c.kind, c.media FROM person_kinds c JOIN event_kinds k ON k.kind = c.kind
WHERE c.email = ? AND c.kind = ? AND k.producer = ?`, email, kind, producer))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SetChoice keeps how a person asked to be told of a kind. The person
// must be in the directory and the kind declared.
func (s *Store) SetChoice(ctx context.Context, email string, c notice.Choice, at time.Time) error {
	media := c.Media
	if media == nil {
		media = []notice.Medium{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO person_kinds (email, kind, media, set_at) VALUES (?, ?, ?, ?)
ON CONFLICT (email, kind) DO UPDATE SET media = excluded.media, set_at = excluded.set_at`, email, c.Kind, jsonOf(media), fmtTime(at))
	return err
}

// ClearChoice puts a kind back on the person's rules by weight.
func (s *Store) ClearChoice(ctx context.Context, email, kind string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM person_kinds WHERE email = ? AND kind = ?`, email, kind)
	return err
}
