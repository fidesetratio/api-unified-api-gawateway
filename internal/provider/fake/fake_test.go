package fake

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/provider"
	"unifiedgw/internal/secrets"
)

var _ provider.Provider = (*Provider)(nil)

func req(key string, settings map[string]any) provider.CreateRequest {
	amt, _ := domain.ParseAmount("10.00", "USD")
	return provider.CreateRequest{Reference: "tx-1", IdempotencyKey: key, Amount: amt, Settings: settings}
}

func TestCreateSuccess(t *testing.T) {
	p := New()
	res, err := p.CreatePayment(context.Background(), req("k1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.ProviderRef, "FAKE-") || !strings.Contains(res.PaymentURL, res.ProviderRef) || res.ExpiresAt == nil {
		t.Errorf("unexpected result %+v", res)
	}
}

func TestSameIdempotencyKeyIsOnePayment(t *testing.T) {
	p := New()
	a, _ := p.CreatePayment(context.Background(), req("k1", nil))
	b, _ := p.CreatePayment(context.Background(), req("k1", nil))
	c, _ := p.CreatePayment(context.Background(), req("k2", nil))
	if a.ProviderRef != b.ProviderRef || a.ProviderRef == c.ProviderRef {
		t.Errorf("refs: %s %s %s", a.ProviderRef, b.ProviderRef, c.ProviderRef)
	}
	if p.CreateCalls() != 3 || p.UniquePayments() != 2 {
		t.Errorf("calls=%d unique=%d", p.CreateCalls(), p.UniquePayments())
	}
}

func TestConcurrentCreatesSameKey(t *testing.T) {
	p := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.CreatePayment(context.Background(), req("same", nil)) }()
	}
	wg.Wait()
	if p.UniquePayments() != 1 {
		t.Errorf("unique payments = %d, want 1", p.UniquePayments())
	}
}

func TestBehaviors(t *testing.T) {
	p := New()
	if _, err := p.CreatePayment(context.Background(), req("a", map[string]any{"behavior": BehaviorTimeout})); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("timeout: %v", err)
	}
	var perr *provider.Error
	if _, err := p.CreatePayment(context.Background(), req("b", map[string]any{"behavior": BehaviorReject})); !errors.As(err, &perr) {
		t.Errorf("reject: %v", err)
	}
	start := time.Now()
	if _, err := p.CreatePayment(context.Background(), req("c", map[string]any{"behavior": BehaviorSlow, "delay_ms": float64(60)})); err != nil {
		t.Errorf("slow: %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("slow behavior did not delay")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.CreatePayment(ctx, req("d", map[string]any{"behavior": BehaviorSlow, "delay_ms": float64(500)})); !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("slow with cancelled ctx: %v", err)
	}
	if p.UniquePayments() != 1 { // only "c" succeeded
		t.Errorf("unique = %d", p.UniquePayments())
	}
}

func TestGetStatus(t *testing.T) {
	p := New()
	res, _ := p.CreatePayment(context.Background(), req("k", nil))
	st, err := p.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef})
	if err != nil || st.Status != domain.StatusPending {
		t.Fatalf("%+v %v", st, err)
	}
	p.SetStatus(res.ProviderRef, domain.StatusPaid)
	st, _ = p.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: res.ProviderRef})
	if st.Status != domain.StatusPaid {
		t.Errorf("status = %s", st.Status)
	}
	if _, err := p.GetStatus(context.Background(), provider.StatusRequest{ProviderRef: "nope"}); err == nil {
		t.Error("unknown ref should fail")
	}
}

func webhook(token, body string) provider.WebhookRequest {
	h := http.Header{}
	if token != "" {
		h.Set("X-Fake-Token", token)
	}
	return provider.WebhookRequest{
		Headers: h, Body: []byte(body),
		WebhookSecret: provider.Credentials{"token": secrets.New("s3cret")},
	}
}

func TestVerifyWebhook(t *testing.T) {
	p := New()
	good := `{"event_id":"E1","provider_ref":"FAKE-1","status":"paid","amount":"10.00","currency":"USD"}`

	ev, err := p.VerifyWebhook(context.Background(), webhook("s3cret", good))
	if err != nil || ev.EventID != "E1" || ev.Status != domain.StatusPaid || ev.Amount == nil || ev.Amount.String() != "10.00" {
		t.Fatalf("%+v %v", ev, err)
	}

	for name, r := range map[string]provider.WebhookRequest{
		"wrong token":   webhook("nope", good),
		"missing token": webhook("", good),
		"bad json":      webhook("s3cret", `{`),
		"no event id":   webhook("s3cret", `{"provider_ref":"x","status":"paid"}`),
		"bad status":    webhook("s3cret", `{"event_id":"E","provider_ref":"x","status":"weird"}`),
		"bad amount":    webhook("s3cret", `{"event_id":"E","provider_ref":"x","status":"paid","amount":"-1","currency":"USD"}`),
	} {
		if _, err := p.VerifyWebhook(context.Background(), r); !errors.Is(err, provider.ErrNotVerified) {
			t.Errorf("%s: got %v", name, err)
		}
	}

	// An empty configured secret must never authenticate, even with an empty header.
	r := webhook("", good)
	r.WebhookSecret = provider.Credentials{}
	if _, err := p.VerifyWebhook(context.Background(), r); !errors.Is(err, provider.ErrNotVerified) {
		t.Errorf("empty secret authenticated: %v", err)
	}
}
