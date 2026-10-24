package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/auth"
	"unifiedgw/internal/domain"
)

// Clients manages API clients. Reads are used by the server; writes by gatewayctl.
type Clients struct{ pool *pgxpool.Pool }

// NewClients builds the client store.
func NewClients(pool *pgxpool.Pool) *Clients { return &Clients{pool: pool} }

// FindByKeyID implements auth.Directory.
func (c *Clients) FindByKeyID(ctx context.Context, keyID string) (auth.Record, error) {
	var rec auth.Record
	var status string
	err := c.pool.QueryRow(ctx,
		`SELECT id, name, role, key_hash, status FROM api_clients WHERE key_id = $1`, keyID,
	).Scan(&rec.Principal.ID, &rec.Principal.Name, &rec.Principal.Role, &rec.Hash, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Record{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.Record{}, err
	}
	rec.Active = status == "active"
	return rec, nil
}

// Create inserts a client and returns its plaintext key, which is not stored.
func (c *Clients) Create(ctx context.Context, name, role string) (auth.Principal, string, error) {
	if role != auth.RoleCaller && role != auth.RoleAdmin {
		return auth.Principal{}, "", domain.ErrInvalidRequest
	}
	key, keyID, hash, err := auth.NewKey()
	if err != nil {
		return auth.Principal{}, "", err
	}
	p := auth.Principal{Name: name, Role: role}
	err = c.pool.QueryRow(ctx,
		`INSERT INTO api_clients (name, role, key_id, key_hash) VALUES ($1,$2,$3,$4) RETURNING id`,
		name, role, keyID, hash).Scan(&p.ID)
	if err != nil {
		return auth.Principal{}, "", err
	}
	return p, key, nil
}

// RotateKey replaces a client's key and returns the new plaintext key.
func (c *Clients) RotateKey(ctx context.Context, name string) (string, error) {
	key, keyID, hash, err := auth.NewKey()
	if err != nil {
		return "", err
	}
	tag, err := c.pool.Exec(ctx,
		`UPDATE api_clients SET key_id=$2, key_hash=$3, updated_at=now() WHERE name=$1`, name, keyID, hash)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", domain.ErrConfigNotFound
	}
	return key, nil
}

// SetStatus enables or disables a client by name.
func (c *Clients) SetStatus(ctx context.Context, name, status string) error {
	tag, err := c.pool.Exec(ctx,
		`UPDATE api_clients SET status=$2, updated_at=now() WHERE name=$1`, name, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConfigNotFound
	}
	return nil
}

// ClientRow is a client without any key material.
type ClientRow struct {
	ID         int64
	Name, Role string
	Status     string
}

// List returns all clients ordered by id.
func (c *Clients) List(ctx context.Context) ([]ClientRow, error) {
	rows, err := c.pool.Query(ctx, `SELECT id, name, role, status FROM api_clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientRow
	for rows.Next() {
		var r ClientRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Role, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoleOf returns a client's role; ok=false when the client does not exist.
func (c *Clients) RoleOf(ctx context.Context, clientID int64) (role string, ok bool, err error) {
	err = c.pool.QueryRow(ctx, `SELECT role FROM api_clients WHERE id=$1`, clientID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return role, err == nil, err
}

// HasAccess reports whether the client has been granted the config.
func (c *Clients) HasAccess(ctx context.Context, clientID, configID int64) (bool, error) {
	var ok bool
	err := c.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM client_config_access WHERE client_id=$1 AND provider_config_id=$2)`,
		clientID, configID).Scan(&ok)
	return ok, err
}
