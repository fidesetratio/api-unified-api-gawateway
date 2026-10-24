-- Append-only record of administrative actions. Never contains secrets.
CREATE TABLE audit_log (
  id         bigserial   PRIMARY KEY,
  actor_type text        NOT NULL CHECK (actor_type IN ('admin', 'operator', 'system')),
  actor_id   text        NOT NULL CHECK (actor_id <> ''),
  action     text        NOT NULL,
  target     text,
  details    jsonb       NOT NULL DEFAULT '{}'::jsonb,
  at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_at ON audit_log (at);
