package provider

import (
	"context"
	"errors"
	"testing"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(NewStub("paypal")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(NewStub("paypal")); err == nil {
		t.Error("duplicate registration must fail")
	}
	if p, ok := r.Get("paypal"); !ok || p.Name() != "paypal" {
		t.Error("registered provider not found")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("unknown provider found")
	}
}

func TestStubIsNotImplemented(t *testing.T) {
	s := NewStub("xendit")
	if _, err := s.CreatePayment(context.Background(), CreateRequest{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("got %v", err)
	}
	if _, err := s.VerifyWebhook(context.Background(), WebhookRequest{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("got %v", err)
	}
}
