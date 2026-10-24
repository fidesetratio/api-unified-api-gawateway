package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/secrets"
)

// WriteAudit appends one audit row outside of another store operation. details
// must never contain secrets.
func WriteAudit(ctx context.Context, pool *pgxpool.Pool, who Actor, action, target string, details map[string]any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := addAudit(ctx, tx, who, action, target, details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReviewRow is a review item with its payment reference, for operators.
type ReviewRow struct {
	ID            int64
	TransactionID string
	UniqueCode    string
	Kind          string
	State         string
	Details       string
}

// ListOpenReviews returns every open review item, oldest first.
func ListOpenReviews(ctx context.Context, pool *pgxpool.Pool) ([]ReviewRow, error) {
	rows, err := pool.Query(ctx, `SELECT r.id, r.transaction_id::text, t.uniquecode, r.kind, r.state, r.details::text
		FROM review_items r JOIN payment_transactions t ON t.id = r.transaction_id
		WHERE r.state='open' ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewRow
	for rows.Next() {
		var r ReviewRow
		if err := rows.Scan(&r.ID, &r.TransactionID, &r.UniqueCode, &r.Kind, &r.State, &r.Details); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveReview closes an open review item, recording who and why, and audits it.
func ResolveReview(ctx context.Context, pool *pgxpool.Pool, id int64, resolution string, who Actor) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, `UPDATE review_items SET state='resolved', resolved_by=$2, resolution=$3, resolved_at=now()
		WHERE id=$1 AND state='open'`, id, who.ID, resolution)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(404, "review_not_found", "no open review item with that id")
	}
	if err := addAudit(ctx, tx, who, "review.resolve", fmt.Sprint(id), map[string]any{"resolution": resolution}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReencryptAll re-encrypts every credential not already under the active master
// key, preserving versions. It returns how many rows were rewritten.
func ReencryptAll(ctx context.Context, pool *pgxpool.Pool, keys *secrets.KeyRing, who Actor) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	type row struct {
		configID int64
		version  int
		provider string
		cred, wh []byte
		keyID    string
	}
	rows, err := tx.Query(ctx, `SELECT pc.provider_config_id, pc.version, c.provider, pc.ciphertext, pc.webhook_ciphertext, pc.key_id
		FROM provider_credentials pc JOIN provider_configs c ON c.id = pc.provider_config_id
		WHERE pc.key_id <> $1 FOR UPDATE OF pc`, keys.ActiveID())
	if err != nil {
		return 0, err
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.configID, &r.version, &r.provider, &r.cred, &r.wh, &r.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, r := range todo {
		credAAD := secrets.CredentialAAD(r.configID, r.version, r.provider, "credentials")
		plain, err := keys.Decrypt(r.keyID, r.cred, credAAD)
		if err != nil {
			return 0, fmt.Errorf("config %d version %d: %w", r.configID, r.version, err)
		}
		newCred, newKey, err := keys.Encrypt(plain, credAAD)
		if err != nil {
			return 0, err
		}
		var newWh []byte
		if len(r.wh) > 0 {
			whAAD := secrets.CredentialAAD(r.configID, r.version, r.provider, "webhook")
			whPlain, err := keys.Decrypt(r.keyID, r.wh, whAAD)
			if err != nil {
				return 0, fmt.Errorf("config %d version %d webhook: %w", r.configID, r.version, err)
			}
			if newWh, _, err = keys.Encrypt(whPlain, whAAD); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET ciphertext=$3, webhook_ciphertext=$4, key_id=$5
			WHERE provider_config_id=$1 AND version=$2`, r.configID, r.version, newCred, newWh, newKey); err != nil {
			return 0, err
		}
	}
	if err := addAudit(ctx, tx, who, "key.reencrypt", keys.ActiveID(), map[string]any{"rows": len(todo)}); err != nil {
		return 0, err
	}
	return len(todo), tx.Commit(ctx)
}

// MigrationVersions lists applied migration versions, oldest first. It returns an
// empty list when the schema_migrations table does not exist yet.
func MigrationVersions(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "42P01" { // undefined_table
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
