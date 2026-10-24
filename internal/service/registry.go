package service

import (
	"unifiedgw/internal/provider"
	"unifiedgw/internal/provider/fake"
	"unifiedgw/internal/provider/midtrans"
	"unifiedgw/internal/provider/paypal"
	"unifiedgw/internal/provider/stripe"
	"unifiedgw/internal/provider/xendit"
)

// NewRegistry builds the provider registry shared by the server and gatewayctl.
// The Fake Provider is registered only when allowFake is true (never in
// production).
func NewRegistry(allowFake bool) (*provider.Registry, error) {
	reg := provider.NewRegistry()
	if allowFake {
		if err := reg.Register(fake.New()); err != nil {
			return nil, err
		}
	}
	if err := reg.Register(paypal.New(paypal.Options{})); err != nil {
		return nil, err
	}
	if err := reg.Register(xendit.New(xendit.Options{})); err != nil {
		return nil, err
	}
	if err := reg.Register(stripe.New(stripe.Options{})); err != nil {
		return nil, err
	}
	if err := reg.Register(midtrans.New(midtrans.Options{})); err != nil {
		return nil, err
	}
	return reg, nil
}
