-- Gateway API clients. Only a hash of the key is stored. A key looks like
-- gw_<key_id>_<secret>; key_id is public and used for lookup.
CREATE TABLE api_clients (
  id          bigserial   PRIMARY KEY,
  name        text        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
  role        text        NOT NULL DEFAULT 'caller' CHECK (role IN ('caller', 'admin')),
  key_id      text        NOT NULL UNIQUE,
  key_hash    bytea       NOT NULL CHECK (length(key_hash) = 32),
  status      text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
