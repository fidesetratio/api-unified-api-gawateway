-- Items a human must look at (money may have moved in a way we did not record).
CREATE TABLE review_items (
  id             bigserial   PRIMARY KEY,
  transaction_id uuid        NOT NULL REFERENCES payment_transactions (id) ON DELETE CASCADE,
  kind           text        NOT NULL CHECK (kind IN
                   ('paid_after_expiry', 'stale_result', 'unknown_outcome_exhausted', 'amount_mismatch')),
  state          text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved')),
  details        jsonb       NOT NULL DEFAULT '{}'::jsonb,
  created_at     timestamptz NOT NULL DEFAULT now(),
  resolved_by    text,
  resolution     text,
  resolved_at    timestamptz,
  CHECK ((state = 'open') = (resolved_at IS NULL))
);
-- One open item of a kind per transaction.
CREATE UNIQUE INDEX review_items_open_unique ON review_items (transaction_id, kind) WHERE state = 'open';
