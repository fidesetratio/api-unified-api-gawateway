-- Every webhook delivery, verified or not. Real adapters arrive in Phase 4;
-- the table exists now so the settlement path is final.
CREATE TABLE webhook_events (
  id                 bigserial   PRIMARY KEY,
  provider           text        NOT NULL,
  event_id           text,
  provider_config_id bigint      REFERENCES provider_configs (id),
  transaction_id     uuid        REFERENCES payment_transactions (id),
  verified           boolean     NOT NULL DEFAULT false,
  outcome            text,
  payload            jsonb,
  received_at        timestamptz NOT NULL DEFAULT now(),
  processed_at       timestamptz,
  CHECK (NOT verified OR event_id IS NOT NULL)
);
-- Duplicate guard. Only verified events count, so an unverified sender cannot
-- poison the key of a real event.
CREATE UNIQUE INDEX webhook_events_dedup ON webhook_events (provider, event_id) WHERE verified;

ALTER TABLE payment_status_history
  ADD CONSTRAINT payment_status_history_event_fk
  FOREIGN KEY (webhook_event_id) REFERENCES webhook_events (id);
