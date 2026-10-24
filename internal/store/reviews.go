package store

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// openReview records a review item. A second open item of the same kind for the
// same transaction is ignored (the unique partial index makes this idempotent).
func openReview(ctx context.Context, tx pgx.Tx, txID, kind string, details map[string]any) error {
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO review_items (transaction_id, kind, details)
		VALUES ($1::uuid,$2,$3::jsonb) ON CONFLICT DO NOTHING`, txID, kind, string(b))
	return err
}

// ReviewItem is a row from review_items.
type ReviewItem struct {
	ID            int64
	TransactionID string
	Kind          string
	State         string
	Details       string
}

// OpenReviews lists open review items for a transaction.
func (s *Payments) OpenReviews(ctx context.Context, txID string) ([]ReviewItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, transaction_id::text, kind, state, details::text
		FROM review_items WHERE transaction_id=$1::uuid AND state='open' ORDER BY id`, txID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewItem
	for rows.Next() {
		var r ReviewItem
		if err := rows.Scan(&r.ID, &r.TransactionID, &r.Kind, &r.State, &r.Details); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
