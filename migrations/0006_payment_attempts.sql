-- One row per provider call. fence_token equals payment_transactions.current_fence
-- at the time the attempt started; a result is accepted only while it still matches.
CREATE TABLE payment_attempts (
  id                       bigserial   PRIMARY KEY,
  transaction_id           uuid        NOT NULL REFERENCES payment_transactions (id) ON DELETE CASCADE,
  attempt_no               integer     NOT NULL CHECK (attempt_no > 0),
  fence_token              bigint      NOT NULL,
  lease_owner              text        NOT NULL,
  lease_expires_at         timestamptz NOT NULL,
  state                    text        NOT NULL DEFAULT 'in_flight'
                             CHECK (state IN ('in_flight', 'succeeded', 'failed', 'unknown')),
  provider_idempotency_key text        NOT NULL,
  error_code               text,
  created_at               timestamptz NOT NULL DEFAULT now(),
  finished_at              timestamptz,
  UNIQUE (transaction_id, attempt_no),
  UNIQUE (transaction_id, fence_token)
);
CREATE INDEX payment_attempts_open ON payment_attempts (lease_expires_at) WHERE state = 'in_flight';
