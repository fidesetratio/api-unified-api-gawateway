CREATE TABLE payment_transactions (
  id                  uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
  client_id           bigint        NOT NULL REFERENCES api_clients (id) ON DELETE RESTRICT,
  provider_config_id  bigint        NOT NULL REFERENCES provider_configs (id) ON DELETE RESTRICT,
  uniquecode          text          NOT NULL CHECK (uniquecode ~ '^[A-Za-z0-9._:-]{1,64}$'),
  amount              numeric(20,4) NOT NULL CHECK (amount > 0),
  currency            char(3)       NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  status              text          NOT NULL CHECK (status IN
                        ('creating', 'pending', 'paid', 'failed', 'expired', 'cancelled', 'refunded')),
  provider_status     text,
  provider_ref        text,
  payment_url         text,
  expires_at          timestamptz,
  paid_at             timestamptz,
  failure_code        text,
  request_fingerprint bytea         NOT NULL CHECK (length(request_fingerprint) = 32),
  credential_version  integer,
  current_fence       bigint        NOT NULL DEFAULT 0,
  created_at          timestamptz   NOT NULL DEFAULT now(),
  updated_at          timestamptz   NOT NULL DEFAULT now(),
  -- uniquecode is unique per caller (decision D1).
  UNIQUE (client_id, uniquecode),
  CHECK (paid_at IS NULL OR status IN ('paid', 'refunded'))
);

-- One provider payment maps to at most one transaction.
CREATE UNIQUE INDEX payment_transactions_provider_ref
  ON payment_transactions (provider_config_id, provider_ref) WHERE provider_ref IS NOT NULL;
CREATE INDEX payment_transactions_expiry ON payment_transactions (expires_at) WHERE status = 'pending';
CREATE INDEX payment_transactions_creating ON payment_transactions (updated_at) WHERE status = 'creating';
