-- Append-only enforcement: history and audit rows can never change or vanish.
CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;

CREATE TRIGGER payment_status_history_immutable BEFORE UPDATE OR DELETE ON payment_status_history
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER payment_status_history_no_truncate BEFORE TRUNCATE ON payment_status_history
  FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER audit_log_immutable BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Privileges. gateway_app is the running server (including the admin API);
-- gateway_ops is the CLI. Neither can run DDL.
-- Schema-level grants follow the schema the migration runs in (public in normal
-- use; a throwaway schema in integration tests).
DO $$
BEGIN
  EXECUTE format('GRANT USAGE ON SCHEMA %I TO gateway_app, gateway_ops', current_schema());
  EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I TO gateway_app, gateway_ops', current_schema());
END $$;

GRANT SELECT ON api_clients TO gateway_app;
GRANT SELECT, INSERT, UPDATE ON api_clients TO gateway_ops;

GRANT SELECT, INSERT, UPDATE ON provider_configs, provider_credentials TO gateway_app, gateway_ops;
GRANT SELECT, INSERT, DELETE ON client_config_access TO gateway_app, gateway_ops;
GRANT SELECT, INSERT, UPDATE ON payment_transactions, payment_attempts, webhook_events, review_items
  TO gateway_app, gateway_ops;
GRANT SELECT, INSERT ON payment_status_history, audit_log TO gateway_app, gateway_ops;
