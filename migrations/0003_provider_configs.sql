-- Provider configuration, kept separate from payment transactions.
CREATE TABLE provider_configs (
  id                        bigserial   PRIMARY KEY,
  name                      text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  provider                  text        NOT NULL CHECK (provider IN ('paypal', 'xendit', 'fake')),
  environment               text        NOT NULL CHECK (environment IN ('sandbox', 'production')),
  status                    text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  allowed_currencies        text[]      NOT NULL CHECK (cardinality(allowed_currencies) > 0),
  settings                  jsonb       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(settings) = 'object'),
  active_credential_version integer,
  created_at                timestamptz NOT NULL DEFAULT now(),
  updated_at                timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, environment, name)
);

-- Which caller may use which configuration.
CREATE TABLE client_config_access (
  client_id          bigint      NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE,
  provider_config_id bigint      NOT NULL REFERENCES provider_configs (id) ON DELETE CASCADE,
  granted_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (client_id, provider_config_id)
);
