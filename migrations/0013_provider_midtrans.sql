-- Allow the Midtrans provider.
ALTER TABLE provider_configs DROP CONSTRAINT provider_configs_provider_check;
ALTER TABLE provider_configs
  ADD CONSTRAINT provider_configs_provider_check
  CHECK (provider IN ('paypal', 'xendit', 'stripe', 'midtrans', 'fake'));
