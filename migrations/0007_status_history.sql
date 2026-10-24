-- Append-only record of every status change (immutability enforced in 0011).
CREATE TABLE payment_status_history (
  id               bigserial   PRIMARY KEY,
  transaction_id   uuid        NOT NULL REFERENCES payment_transactions (id) ON DELETE CASCADE,
  from_status      text,
  to_status        text        NOT NULL,
  reason           text        NOT NULL,
  actor_type       text        NOT NULL CHECK (actor_type IN ('client', 'admin', 'system', 'webhook', 'operator')),
  actor_id         text        NOT NULL,
  webhook_event_id bigint,
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_status_history_tx ON payment_status_history (transaction_id, id);
