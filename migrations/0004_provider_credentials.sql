-- Encrypted provider credentials, one row per version. Plaintext never reaches
-- the database; the master key never lives here.
CREATE TABLE provider_credentials (
  provider_config_id bigint      NOT NULL REFERENCES provider_configs (id) ON DELETE CASCADE,
  version            integer     NOT NULL CHECK (version > 0),
  ciphertext         bytea       NOT NULL,
  webhook_ciphertext bytea,
  key_id             text        NOT NULL,
  status             text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
  created_by         text        NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  retired_at         timestamptz,
  PRIMARY KEY (provider_config_id, version)
);

-- Exactly one active version per config.
CREATE UNIQUE INDEX provider_credentials_one_active
  ON provider_credentials (provider_config_id) WHERE status = 'active';

-- The pointer on provider_configs must reference an existing version. Deferred so
-- a config and its first credentials can be inserted in one transaction.
ALTER TABLE provider_configs
  ADD CONSTRAINT provider_configs_active_credential_fk
  FOREIGN KEY (id, active_credential_version)
  REFERENCES provider_credentials (provider_config_id, version)
  DEFERRABLE INITIALLY DEFERRED;
