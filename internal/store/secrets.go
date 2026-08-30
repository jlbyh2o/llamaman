package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The `secrets` table (DESIGN section 2.2).
//
// This file moves CIPHERTEXT. It never encrypts, never decrypts and never sees a
// token: the AES-GCM box and its 0600 key file are internal/secrets', and the
// separation is what keeps D46's honest limit honest — a database file, a
// `db-backups/` entry or a `VACUUM INTO` snapshot carries the sealed bytes and
// nothing that opens them.
//
// `hint` is the one field here that is human-readable, and it is deliberately
// not the secret: `hf_…AbC` is enough for a person to recognize which token they
// pasted and not enough for anyone to use it.

// Secret is one row of `secrets`.
type Secret struct {
	// Name is 'hf_token' or 'github_token' (§2.2). The column carries no CHECK;
	// the set is closed by model.SecretName.
	Name model.SecretName
	// Nonce and Ciphertext are the AES-GCM box. Neither means anything without
	// `<state_dir>/secret.key`.
	Nonce      []byte
	Ciphertext []byte
	// Hint is the masked form for display — never the secret.
	Hint *string
	// Valid is what the last validation call answered: 1 for a token the
	// upstream API accepted, 0 for one it rejected, NULL for one never checked.
	// Three states, because "we have never asked" and "it was refused" are
	// different sentences on screen.
	Valid *bool
	// ScopeJSON is the scope list the validation reported, verbatim JSON.
	ScopeJSON  *string
	CreatedAt  int64
	UpdatedAt  int64
	LastUsedAt *int64
}

const secretColumns = `name, nonce, ciphertext, hint, valid, scope_json,
	created_at, updated_at, last_used_at`

// Secret reads one row. ErrNotFound means no such secret is stored, which is the
// ordinary state of a host that has never been given a token.
func (s *Store) Secret(ctx context.Context, tx Tx, name model.SecretName) (Secret, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+secretColumns+` FROM secrets WHERE name = ?`, string(name))
	return scanSecret(row)
}

// UpsertSecret writes a sealed secret, replacing whatever was there.
//
// `created_at` survives a replacement: "this host has had a Hugging Face token
// since March" stays true when the token behind it is rotated, and it is
// `updated_at` that moves.
func (s *Store) UpsertSecret(ctx context.Context, tx Tx, sec Secret) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO secrets (name, nonce, ciphertext, hint, valid, scope_json,
		                     created_at, updated_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		  nonce        = excluded.nonce,
		  ciphertext   = excluded.ciphertext,
		  hint         = excluded.hint,
		  valid        = excluded.valid,
		  scope_json   = excluded.scope_json,
		  updated_at   = excluded.updated_at,
		  last_used_at = NULL`,
		string(sec.Name), sec.Nonce, sec.Ciphertext, sec.Hint, boolPtrToInt(sec.Valid),
		sec.ScopeJSON, sec.CreatedAt, sec.UpdatedAt, sec.LastUsedAt)
	if err != nil {
		return fmt.Errorf("upsert secret %s: %w", sec.Name, err)
	}
	return nil
}

// DeleteSecret removes one secret. False means there was none.
func (s *Store) DeleteSecret(ctx context.Context, tx Tx, name model.SecretName) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, string(name))
	if err != nil {
		return false, fmt.Errorf("delete secret %s: %w", name, err)
	}
	return rowsChanged(res)
}

// TouchSecret stamps `last_used_at`. It is what lets the settings screen say
// "last used two minutes ago", which is how a user tells a token that is wired
// up from one that is merely stored.
func (s *Store) TouchSecret(ctx context.Context, tx Tx, name model.SecretName, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE secrets SET last_used_at = ? WHERE name = ?`, at, string(name))
	if err != nil {
		return false, fmt.Errorf("touch secret %s: %w", name, err)
	}
	return rowsChanged(res)
}

// SetSecretValidity records what a validation call answered, without touching
// the sealed bytes: a token the upstream API has started refusing is still the
// token the user gave us, and replacing it is their decision.
func (s *Store) SetSecretValidity(ctx context.Context, tx Tx, name model.SecretName,
	valid bool, scopeJSON *string, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE secrets SET valid = ?, scope_json = ?, updated_at = ? WHERE name = ?`,
		boolInt(valid), scopeJSON, at, string(name))
	if err != nil {
		return false, fmt.Errorf("set the validity of secret %s: %w", name, err)
	}
	return rowsChanged(res)
}

func scanSecret(sc interface{ Scan(...any) error }) (Secret, error) {
	var (
		out   Secret
		name  string
		valid sql.NullInt64
	)
	err := sc.Scan(&name, &out.Nonce, &out.Ciphertext, &out.Hint, &valid, &out.ScopeJSON,
		&out.CreatedAt, &out.UpdatedAt, &out.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, fmt.Errorf("scan secret: %w", err)
	}
	out.Name = model.SecretName(name)
	if valid.Valid {
		v := valid.Int64 != 0
		out.Valid = &v
	}
	return out, nil
}

// boolPtrToInt renders a three-state boolean for a nullable CHECK (0,1) column:
// NULL means "never asked", which is a different answer from "refused".
func boolPtrToInt(b *bool) any {
	if b == nil {
		return nil
	}
	return boolInt(*b)
}
