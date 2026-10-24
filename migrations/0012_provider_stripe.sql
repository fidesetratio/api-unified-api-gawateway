-- Allow the Stripe provider. The CHECK on provider_configs.provider was created
-- inline in 0003, so it carries PostgreSQL's generated name.
ALTER TABLE provider_configs DROP CONSTRAINT provider_configs_provider_check;
ALTER TABLE provider_configs
  ADD CONSTRAINT provider_configs_provider_check
  CHECK (provider IN ('paypal', 'xendit', 'stripe', 'fake'));
