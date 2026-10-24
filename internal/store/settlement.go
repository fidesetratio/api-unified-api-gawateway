package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"unifiedgw/internal/domain"
)

// Webhook outcomes recorded on webhook_events.outcome.
const (
	OutcomeProcessed        = "processed"
	OutcomeDuplicate        = "duplicate"
	OutcomeIgnored          = "ignored"
	OutcomeUnknownReference = "unknown_reference"
	OutcomeAmountMismatch   = "amount_mismatch"
	OutcomeInvalidTransit   = "invalid_transition"
	OutcomeNotVerified      = "not_verified"
)

// EventInput is a webhook delivery that has already been authenticated by the
// provider adapter. Nothing here is applied before that verification.
type EventInput struct {
	Provider    string
	ConfigID    int64
	EventID     string
	ProviderRef string
	Status      domain.Status
	Amount      *domain.Amount // nil when the provider reported none
	Ignore      bool           // verified but of no use: record and acknowledge only
	Payload     string         // scrubbed JSON, may be empty
}

// RecordUnverified stores a delivery that failed verification, for forensics.
// It never changes a transaction.
func (s *Payments) RecordUnverified(ctx context.Context, provider string, configID int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO webhook_events (provider, provider_config_id, verified, outcome, processed_at)
		VALUES ($1,$2,false,$3,now())`, provider, configID, OutcomeNotVerified)
	return err
}

// ApplyEvent applies a verified event atomically: dedup by event id, locate the
// transaction, check amount and currency against what we stored, validate the
// transition, then write status, history and (when needed) a review item. It
// returns the outcome; err is only for failures the provider should retry.
func (s *Payments) ApplyEvent(ctx context.Context, in EventInput) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var eventRow int64
	err = tx.QueryRow(ctx, `INSERT INTO webhook_events (provider, event_id, provider_config_id, verified, payload)
		VALUES ($1,$2,$3,true,NULLIF($4,'')::jsonb)
		ON CONFLICT (provider, event_id) WHERE verified DO NOTHING RETURNING id`,
		in.Provider, in.EventID, in.ConfigID, in.Payload).Scan(&eventRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutcomeDuplicate, nil // already handled; nothing to commit
	}
	if err != nil {
		return "", err
	}

	finish := func(outcome string, txID *string) (string, error) {
		if _, err := tx.Exec(ctx, `UPDATE webhook_events SET outcome=$2, transaction_id=$3::uuid, processed_at=now()
			WHERE id=$1`, eventRow, outcome, txID); err != nil {
			return "", err
		}
		return outcome, tx.Commit(ctx)
	}

	if in.Ignore {
		return finish(OutcomeIgnored, nil)
	}

	var p Payment
	var status string
	err = tx.QueryRow(ctx, `SELECT id::text, amount::text, currency, status FROM payment_transactions
		WHERE provider_config_id=$1 AND provider_ref=$2 FOR UPDATE`, in.ConfigID, in.ProviderRef,
	).Scan(&p.ID, &p.Amount, &p.Currency, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return finish(OutcomeUnknownReference, nil)
	}
	if err != nil {
		return "", err
	}
	p.Status = domain.Status(status)
	id := p.ID

	// A payment is only ever marked paid on a verified event whose amount and
	// currency equal what we stored.
	stored, perr := domain.ParseAmount(p.Amount, p.Currency)
	if perr != nil {
		return "", perr
	}
	if in.Status == domain.StatusPaid && (in.Amount == nil || !in.Amount.Equal(stored)) {
		if err := openReview(ctx, tx, id, "amount_mismatch", map[string]any{
			"event_id": in.EventID, "provider": in.Provider, "expected": stored.String(), "currency": stored.Currency(),
			"reported_present": in.Amount != nil,
		}); err != nil {
			return "", err
		}
		return finish(OutcomeAmountMismatch, &id)
	}

	switch domain.CheckTransition(p.Status, in.Status) {
	case domain.OutcomeNoop:
		return finish(OutcomeIgnored, &id)
	case domain.OutcomeRejected:
		return finish(OutcomeInvalidTransit, &id)
	case domain.OutcomeAllowedNeedsReview:
		if err := openReview(ctx, tx, id, "paid_after_expiry", map[string]any{
			"event_id": in.EventID, "was": string(p.Status)}); err != nil {
			return "", err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE payment_transactions
		SET status=$2, paid_at = CASE WHEN $2='paid' THEN now() ELSE paid_at END, updated_at=now()
		WHERE id=$1::uuid`, id, string(in.Status)); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_status_history
		(transaction_id, from_status, to_status, reason, actor_type, actor_id, webhook_event_id)
		VALUES ($1::uuid,$2,$3,$4,'webhook',$5,$6)`,
		id, string(p.Status), string(in.Status), "verified webhook event", in.Provider, eventRow); err != nil {
		return "", err
	}
	return finish(OutcomeProcessed, &id)
}

// ExpireDue moves pending payments whose expiry passed to expired. Rows are
// locked with SKIP LOCKED, so concurrent sweepers never block or double-apply.
func (s *Payments) ExpireDue(ctx context.Context, limit int) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, `SELECT id::text FROM payment_transactions
		WHERE status='pending' AND expires_at IS NOT NULL AND expires_at < now()
		ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET status='expired', updated_at=now() WHERE id=$1::uuid`, id); err != nil {
			return 0, err
		}
		if err := addHistory(ctx, tx, id, string(domain.StatusPending), string(domain.StatusExpired),
			"expiry passed", "system", "expiry-sweeper"); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

// StuckCreating returns creating transactions with no live lease that have not
// moved for at least idle seconds, oldest first.
func (s *Payments) StuckCreating(ctx context.Context, idleSeconds float64, limit int) ([]Payment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payment_transactions t
		WHERE status='creating' AND updated_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (SELECT 1 FROM payment_attempts a
		                  WHERE a.transaction_id = t.id AND a.state='in_flight' AND a.lease_expires_at > now())
		ORDER BY updated_at LIMIT $2`, idleSeconds, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AttemptCount returns how many provider attempts a transaction has had.
func (s *Payments) AttemptCount(ctx context.Context, txID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE transaction_id=$1::uuid`, txID).Scan(&n)
	return n, err
}

// OpenReview opens a review item outside of another store operation.
func (s *Payments) OpenReview(ctx context.Context, txID, kind string, details map[string]any) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := openReview(ctx, tx, txID, kind, details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetByProviderRef returns the payment a provider reference belongs to within a
// config, or domain.ErrPaymentNotFound.
func (s *Payments) GetByProviderRef(ctx context.Context, configID int64, ref string) (Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payment_transactions WHERE provider_config_id=$1 AND provider_ref=$2`, configID, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, domain.ErrPaymentNotFound
	}
	return p, err
}

// FindConfigByProviderRef finds which config of a provider owns a provider
// reference. It is used only to choose the secret an unverified webhook is
// verified with; it never changes anything.
func (s *Payments) FindConfigByProviderRef(ctx context.Context, provider, ref string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT t.provider_config_id FROM payment_transactions t JOIN provider_configs c ON c.id = t.provider_config_id WHERE c.provider=$1 AND t.provider_ref=$2 ORDER BY t.id LIMIT 1`, provider, ref).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}
