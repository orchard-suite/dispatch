package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Token is an API credential dispatch issued. Only its hash is kept.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// Person, when set, is who the token acts as: a change it makes is that
	// person's, as on the desk.
	Person string `json:"person,omitempty"`
}

func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// MintToken makes a token and returns its secret once.
func (s *Store) MintToken(ctx context.Context, name, by string) (Token, string, error) {
	return s.mint(ctx, name, by, "")
}

// MintPersonToken makes a token that acts as one person, for their own
// agents, and returns its secret once.
func (s *Store) MintPersonToken(ctx context.Context, name, person string) (Token, string, error) {
	return s.mint(ctx, name, person, person)
}

func (s *Store) mint(ctx context.Context, name, by, person string) (Token, string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, "", err
	}
	secret := "dsp_" + base64.RawURLEncoding.EncodeToString(raw)
	idRaw := make([]byte, 6)
	if _, err := rand.Read(idRaw); err != nil {
		return Token{}, "", err
	}
	t := Token{ID: hex.EncodeToString(idRaw), Name: name, CreatedAt: time.Now(), CreatedBy: by, Person: person}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (id, name, hash, created_at, created_by, person) VALUES (?, ?, ?, ?, ?, ?)`, t.ID, t.Name, hashToken(secret), fmtTime(t.CreatedAt), t.CreatedBy, t.Person)
	return t, secret, err
}

// VerifyToken finds the token a secret belongs to and notes the use.
func (s *Store) VerifyToken(ctx context.Context, secret string) (Token, bool) {
	h := hashToken(secret)
	var t Token
	var created string
	var last sql.NullString
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, hash, created_at, created_by, last_used_at, person FROM tokens WHERE hash = ?`, h).Scan(&t.ID, &t.Name, &stored, &created, &t.CreatedBy, &last, &t.Person)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 {
		return Token{}, false
	}
	t.CreatedAt = parseTime(created)
	t.LastUsedAt = parseTimePtr(last)
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, fmtTime(time.Now()), t.ID)
	return t, true
}

func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, created_by, last_used_at, person FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created string
		var last sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &created, &t.CreatedBy, &last, &t.Person); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(created)
		t.LastUsedAt = parseTimePtr(last)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountTokens(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tokens`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
