-- Application roles. They are NOLOGIN here; an operator gives them a password
-- (or maps a login role into them) outside of migrations so no secret lives in SQL.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_app') THEN
    CREATE ROLE gateway_app NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_ops') THEN
    CREATE ROLE gateway_ops NOLOGIN;
  END IF;
END $$;
