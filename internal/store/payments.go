package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/domain"
)

// Payment is a stored payment transaction.
type Payment struct {
	ID                string
	ClientID          int64
	ConfigID          int64
	UniqueCode        string
	Amount            string
	Currency          string
	Status            domain.Status
	ProviderRef       string
	PaymentURL        string
	ExpiresAt         *time.Time
	Fingerprint       []byte
	CredentialVersion *int
	Fence             int64
	CreatedAt         time.Time
}

// NewPayment holds the fields needed to create a transaction row.
type NewPayment struct {
	ClientID          int64
	ConfigID          int64
	UniqueCode        string
	Amount            domain.Amount
	Fingerprint       []byte
	CredentialVersion *int
}

// Attempt is one provider call, fenced by Fence.
type Attempt struct {
	ID             int64
	TransactionID  string
	Fence          int64
	Owner          string
	IdempotencyKey string
}

// Errors returned by attempt handling.
var (
	// ErrAttemptInFlight means another worker holds a live lease.
	ErrAttemptInFlight = errors.New("an attempt is already in flight")
	// ErrNotCreating means the transaction already left the creating state.
	ErrNotCreating = errors.New("transaction is not in the creating state")
)

// Payments stores transactions, attempts, history and review items.
type Payments struct{ pool *pgxpool.Pool }

// NewPayments builds the payment store.
func NewPayments(pool *pgxpool.Pool) *Payments { return &Payments{pool: pool} }

const paymentColumns = `id::text, client_id, provider_config_id, uniquecode, amount::text, currency, status,
	coalesce(provider_ref,''), coalesce(payment_url,''), expires_at, request_fingerprint, credential_version, current_fence, created_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	var status string
	err := row.Scan(&p.ID, &p.ClientID, &p.ConfigID, &p.UniqueCode, &p.Amount, &p.Currency, &status,
		&p.ProviderRef, &p.PaymentURL, &p.ExpiresAt, &p.Fingerprint, &p.CredentialVersion, &p.Fence, &p.CreatedAt)
	p.Status = domain.Status(status)
	return p, err
}

// InsertOrGet creates the transaction in the creating state. When the caller
// already used this uniquecode it returns the existing row and inserted=false;
// concurrent identical requests therefore converge on a single row.
func (s *Payments) InsertOrGet(ctx context.Context, n NewPayment) (Payment, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO payment_transactions
		  (client_id, provider_config_id, uniquecode, amount, currency, status, request_fingerprint, credential_version)
		VALUES ($1,$2,$3,$4::numeric,$5,'creating',$6,$7)
		ON CONFLICT (client_id, uniquecode) DO NOTHING
		RETURNING `+paymentColumns,
		n.ClientID, n.ConfigID, n.UniqueCode, n.Amount.String(), n.Amount.Currency(), n.Fingerprint, n.CredentialVersion)
	p, err := scanPayment(row)
	if err == nil {
		if err := addHistory(ctx, tx, p.ID, "", string(domain.StatusCreating), "created", "client", itoa(n.ClientID)); err != nil {
			return Payment{}, false, err
		}
		return p, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, false, err
	}
	existing, err := scanPayment(tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payment_transactions WHERE client_id=$1 AND uniquecode=$2`,
		n.ClientID, n.UniqueCode))
	if err != nil {
		return Payment{}, false, err
	}
	return existing, false, tx.Commit(ctx)
}

// GetByCode returns a caller's payment by uniquecode, or domain.ErrPaymentNotFound.
func (s *Payments) GetByCode(ctx context.Context, clientID int64, code string) (Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payment_transactions WHERE client_id=$1 AND uniquecode=$2`, clientID, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, domain.ErrPaymentNotFound
	}
	return p, err
}

// GetByID returns a payment by id, or domain.ErrPaymentNotFound.
func (s *Payments) GetByID(ctx context.Context, id string) (Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payment_transactions WHERE id=$1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, domain.ErrPaymentNotFound
	}
	return p, err
}

// BeginAttempt takes the lease for a creating transaction. It locks the row, so
// two workers cannot both start; an unexpired in-flight attempt blocks takeover
// with ErrAttemptInFlight. Any older in-flight attempt is marked unknown and its
// fence token is superseded, so its late result will be rejected.
func (s *Payments) BeginAttempt(ctx context.Context, txID, owner string, lease time.Duration) (Attempt, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM payment_transactions WHERE id=$1::uuid FOR UPDATE`, txID).Scan(&status); err != nil {
		return Attempt{}, err
	}
	if domain.Status(status) != domain.StatusCreating {
		return Attempt{}, ErrNotCreating
	}

	var live bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_attempts
		WHERE transaction_id=$1::uuid AND state='in_flight' AND lease_expires_at > now())`, txID).Scan(&live); err != nil {
		return Attempt{}, err
	}
	if live {
		return Attempt{}, ErrAttemptInFlight
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='unknown', finished_at=now()
		WHERE transaction_id=$1::uuid AND state='in_flight'`, txID); err != nil {
		return Attempt{}, err
	}

	var fence int64
	if err := tx.QueryRow(ctx, `UPDATE payment_transactions SET current_fence = current_fence + 1, updated_at = now()
		WHERE id=$1::uuid RETURNING current_fence`, txID).Scan(&fence); err != nil {
		return Attempt{}, err
	}
	a := Attempt{TransactionID: txID, Fence: fence, Owner: owner, IdempotencyKey: txID}
	if err := tx.QueryRow(ctx, `INSERT INTO payment_attempts
		(transaction_id, attempt_no, fence_token, lease_owner, lease_expires_at, provider_idempotency_key)
		VALUES ($1::uuid, $2::bigint, $2::bigint, $3, now() + make_interval(secs => $4), $5) RETURNING id`,
		txID, fence, owner, lease.Seconds(), a.IdempotencyKey).Scan(&a.ID); err != nil {
		return Attempt{}, err
	}
	return a, tx.Commit(ctx)
}

// ProviderResult is the success data written by CompleteSuccess.
type ProviderResult struct {
	ProviderRef string
	PaymentURL  string
	ExpiresAt   *time.Time
	RawStatus   string
}

// CompleteSuccess records a provider success and moves creating -> pending, but
// only if the attempt's fence token is still current. applied=false means the
// result is stale: it is discarded and a stale_result review item is opened so a
// human can check whether a provider payment exists that we did not record.
func (s *Payments) CompleteSuccess(ctx context.Context, a Attempt, r ProviderResult) (applied bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `UPDATE payment_transactions
		SET status='pending', provider_ref=$3, payment_url=$4, expires_at=$5, provider_status=$6, updated_at=now()
		WHERE id=$1::uuid AND current_fence=$2 AND status='creating'`,
		a.TransactionID, a.Fence, r.ProviderRef, r.PaymentURL, r.ExpiresAt, r.RawStatus)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='succeeded', finished_at=now() WHERE id=$1 AND state='in_flight'`, a.ID); err != nil {
			return false, err
		}
		if err := openReview(ctx, tx, a.TransactionID, "stale_result",
			map[string]any{"attempt_id": a.ID, "fence": a.Fence, "provider_ref": r.ProviderRef}); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='succeeded', finished_at=now() WHERE id=$1`, a.ID); err != nil {
		return false, err
	}
	if err := addHistory(ctx, tx, a.TransactionID, string(domain.StatusCreating), string(domain.StatusPending),
		"provider accepted", "system", a.Owner); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// CompleteFailure records a definite provider rejection and moves creating ->
// failed, subject to the same fence check as CompleteSuccess.
func (s *Payments) CompleteFailure(ctx context.Context, a Attempt, code string) (applied bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `UPDATE payment_transactions
		SET status='failed', failure_code=$3, updated_at=now()
		WHERE id=$1::uuid AND current_fence=$2 AND status='creating'`, a.TransactionID, a.Fence, code)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='failed', error_code=$2, finished_at=now()
		WHERE id=$1 AND state='in_flight'`, a.ID, code); err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if err := addHistory(ctx, tx, a.TransactionID, string(domain.StatusCreating), string(domain.StatusFailed),
		"provider rejected: "+code, "system", a.Owner); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// CompleteUnknown marks an attempt whose outcome is unknown (timeout). The
// transaction stays in creating so a retry or the reconciler can resolve it.
func (s *Payments) CompleteUnknown(ctx context.Context, a Attempt, code string) error {
	_, err := s.pool.Exec(ctx, `UPDATE payment_attempts SET state='unknown', error_code=$2, finished_at=now()
		WHERE id=$1 AND state='in_flight'`, a.ID, code)
	return err
}

func addHistory(ctx context.Context, tx pgx.Tx, txID, from, to, reason, actorType, actorID string) error {
	var fromArg any
	if from != "" {
		fromArg = from
	}
	_, err := tx.Exec(ctx, `INSERT INTO payment_status_history
		(transaction_id, from_status, to_status, reason, actor_type, actor_id)
		VALUES ($1::uuid,$2,$3,$4,$5,$6)`, txID, fromArg, to, reason, actorType, actorID)
	return err
}
