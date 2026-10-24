package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/domain"
)

// Config is a provider configuration without any secret material.
type Config struct {
	ID                int64
	Name              string
	Provider          string
	Environment       string
	Status            string
	AllowedCurrencies []string
	Settings          map[string]any
	ActiveVersion     *int
	HasWebhookSecret  bool
}

// CredentialRow is one encrypted credential version.
type CredentialRow struct {
	Version           int
	Ciphertext        []byte
	WebhookCiphertext []byte
	KeyID             string
}

// Sealer encrypts the plaintext credentials for (configID, version). It is
// called inside the storing transaction because the config id is only known
// after the insert. It returns ciphertexts and the master key id used.
type Sealer func(configID int64, version int) (cred, webhook []byte, keyID string, err error)

// NewConfig describes a config to create.
type NewConfig struct {
	Name              string
	Provider          string
	Environment       string
	AllowedCurrencies []string
	Settings          map[string]any
}

// Actor identifies who performed an administrative action, for the audit log.
type Actor struct {
	Type string // admin | operator | system
	ID   string
}

// Configs stores provider configurations, credentials and grants.
type Configs struct{ pool *pgxpool.Pool }

// NewConfigs builds the config store.
func NewConfigs(pool *pgxpool.Pool) *Configs { return &Configs{pool: pool} }

const configColumns = `c.id, c.name, c.provider, c.environment, c.status, c.allowed_currencies, c.settings,
	c.active_credential_version,
	coalesce((SELECT pc.webhook_ciphertext IS NOT NULL FROM provider_credentials pc
	          WHERE pc.provider_config_id = c.id AND pc.version = c.active_credential_version), false)`

func scanConfig(row pgx.Row) (Config, error) {
	var c Config
	var settings []byte
	err := row.Scan(&c.ID, &c.Name, &c.Provider, &c.Environment, &c.Status, &c.AllowedCurrencies,
		&settings, &c.ActiveVersion, &c.HasWebhookSecret)
	if err != nil {
		return Config{}, err
	}
	c.Settings = map[string]any{}
	if len(settings) > 0 {
		if err := json.Unmarshal(settings, &c.Settings); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

// Get returns a config by id, or domain.ErrConfigNotFound.
func (s *Configs) Get(ctx context.Context, id int64) (Config, error) {
	c, err := scanConfig(s.pool.QueryRow(ctx, `SELECT `+configColumns+` FROM provider_configs c WHERE c.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, domain.ErrConfigNotFound
	}
	return c, err
}

// List returns all configs ordered by id.
func (s *Configs) List(ctx context.Context) ([]Config, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+configColumns+` FROM provider_configs c ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Config
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveCredentials returns the active encrypted credential version.
func (s *Configs) ActiveCredentials(ctx context.Context, configID int64) (CredentialRow, error) {
	var r CredentialRow
	err := s.pool.QueryRow(ctx, `SELECT version, ciphertext, webhook_ciphertext, key_id
		FROM provider_credentials WHERE provider_config_id=$1 AND status='active'`, configID,
	).Scan(&r.Version, &r.Ciphertext, &r.WebhookCiphertext, &r.KeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialRow{}, domain.ErrConfigNotFound
	}
	return r, err
}

// Create inserts a config and its first credentials atomically and writes an
// audit row. It returns the new config.
func (s *Configs) Create(ctx context.Context, n NewConfig, seal Sealer, who Actor) (Config, error) {
	settings, err := json.Marshal(nonNil(n.Settings))
	if err != nil {
		return Config{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Config{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO provider_configs
		(name, provider, environment, allowed_currencies, settings, active_credential_version)
		VALUES ($1,$2,$3,$4,$5::jsonb,1) RETURNING id`,
		n.Name, n.Provider, n.Environment, n.AllowedCurrencies, string(settings)).Scan(&id)
	if err != nil {
		return Config{}, err
	}
	cred, webhook, keyID, err := seal(id, 1)
	if err != nil {
		return Config{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_credentials
		(provider_config_id, version, ciphertext, webhook_ciphertext, key_id, created_by)
		VALUES ($1,1,$2,$3,$4,$5)`, id, cred, webhook, keyID, who.ID); err != nil {
		return Config{}, err
	}
	if err := addAudit(ctx, tx, who, "config.create", itoa(id), map[string]any{
		"provider": n.Provider, "environment": n.Environment, "name": n.Name}); err != nil {
		return Config{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Config{}, err
	}
	return s.Get(ctx, id)
}

// RotateCredentials adds a new credential version and makes it active. The old
// version is kept (retired) so in-flight attempts can finish.
func (s *Configs) RotateCredentials(ctx context.Context, configID int64, seal Sealer, who Actor) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var cur int
	err = tx.QueryRow(ctx, `SELECT active_credential_version FROM provider_configs WHERE id=$1 FOR UPDATE`, configID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrConfigNotFound
	}
	if err != nil {
		return 0, err
	}
	next := cur + 1
	cred, webhook, keyID, err := seal(configID, next)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET status='retired', retired_at=now()
		WHERE provider_config_id=$1 AND version=$2`, configID, cur); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_credentials
		(provider_config_id, version, ciphertext, webhook_ciphertext, key_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)`, configID, next, cred, webhook, keyID, who.ID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_configs SET active_credential_version=$2, updated_at=now() WHERE id=$1`,
		configID, next); err != nil {
		return 0, err
	}
	if err := addAudit(ctx, tx, who, "config.rotate_credentials", itoa(configID), map[string]any{"version": next}); err != nil {
		return 0, err
	}
	return next, tx.Commit(ctx)
}

// ConfigUpdate lists fields that may change; nil means unchanged.
type ConfigUpdate struct {
	Name              *string
	AllowedCurrencies []string
	Settings          map[string]any
	Status            *string
}

// Update changes a config's non-secret fields and audits the change.
func (s *Configs) Update(ctx context.Context, id int64, u ConfigUpdate, who Actor) (Config, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Config{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var settings any
	if u.Settings != nil {
		b, err := json.Marshal(u.Settings)
		if err != nil {
			return Config{}, err
		}
		settings = string(b)
	}
	var currencies any
	if u.AllowedCurrencies != nil {
		currencies = u.AllowedCurrencies
	}
	tag, err := tx.Exec(ctx, `UPDATE provider_configs SET
		name = coalesce($2, name),
		allowed_currencies = coalesce($3, allowed_currencies),
		settings = coalesce($4::jsonb, settings),
		status = coalesce($5, status),
		updated_at = now()
		WHERE id=$1`, id, u.Name, currencies, settings, u.Status)
	if err != nil {
		return Config{}, err
	}
	if tag.RowsAffected() == 0 {
		return Config{}, domain.ErrConfigNotFound
	}
	if err := addAudit(ctx, tx, who, "config.update", itoa(id), map[string]any{
		"name_changed": u.Name != nil, "currencies_changed": u.AllowedCurrencies != nil,
		"settings_changed": u.Settings != nil, "status": u.Status}); err != nil {
		return Config{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Config{}, err
	}
	return s.Get(ctx, id)
}

// Grant allows a client to use a config. It is idempotent.
func (s *Configs) Grant(ctx context.Context, configID, clientID int64, who Actor) error {
	return s.changeGrant(ctx, configID, clientID, who, true)
}

// Revoke removes a client's access to a config. It is idempotent.
func (s *Configs) Revoke(ctx context.Context, configID, clientID int64, who Actor) error {
	return s.changeGrant(ctx, configID, clientID, who, false)
}

func (s *Configs) changeGrant(ctx context.Context, configID, clientID int64, who Actor, grant bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	action := "grant.remove"
	if grant {
		action = "grant.add"
		_, err = tx.Exec(ctx, `INSERT INTO client_config_access (client_id, provider_config_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, clientID, configID)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM client_config_access WHERE client_id=$1 AND provider_config_id=$2`,
			clientID, configID)
	}
	if err != nil {
		return err
	}
	if err := addAudit(ctx, tx, who, action, itoa(configID), map[string]any{"client_id": clientID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// addAudit appends an audit row. details must never contain secrets.
func addAudit(ctx context.Context, tx pgx.Tx, who Actor, action, target string, details map[string]any) error {
	b, err := json.Marshal(nonNil(details))
	if err != nil {
		return fmt.Errorf("marshal audit details: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor_type, actor_id, action, target, details)
		VALUES ($1,$2,$3,$4,$5::jsonb)`, who.Type, who.ID, action, target, string(b))
	return err
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
