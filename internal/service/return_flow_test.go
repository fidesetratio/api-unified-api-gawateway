package service_test

import (
	"context"
	"errors"
	"testing"

	"unifiedgw/internal/domain"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
)

func TestReturnPaysOnlyWhenProviderConfirms(t *testing.T) {
	e := newPPEnv(t)
	ctx := context.Background()
	order, _ := e.startPayment(t, "ORDER-R1", "10.00", "USD")
	p, err := e.payments.GetByCode(ctx, e.caller.ID, "ORDER-R1")
	must(t, err)

	// The buyer comes back without having approved: nothing changes, nothing is captured.
	v, err := e.settle.CheckReturn(ctx, p.ID)
	must(t, err)
	if v.Status != domain.StatusPending || v.UniqueCode != "ORDER-R1" {
		t.Fatalf("unapproved return: %+v", v)
	}
	if _, caps, _ := e.srv.Counts(); caps != 0 {
		t.Fatalf("captures = %d, want 0", caps)
	}

	// PayPal reports the order approved: the return captures it exactly once.
	e.srv.Approve(order)
	v, err = e.settle.CheckReturn(ctx, p.ID)
	must(t, err)
	if v.Status != domain.StatusPaid {
		t.Fatalf("after approval: %+v", v)
	}
	if _, err = e.settle.CheckReturn(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, caps, _ := e.srv.Counts(); caps != 1 {
		t.Errorf("captures = %d, want exactly 1", caps)
	}
	if n := e.query(`SELECT count(*)::text FROM payment_status_history WHERE to_status='paid'`); n != "1" {
		t.Errorf("paid history rows = %s", n)
	}
}

func TestReturnRejectsUnknownAndMalformedReferences(t *testing.T) {
	e := newPPEnv(t)
	for _, ref := range []string{"", "x", "ORDER-R1", "00000000-0000-0000-0000-000000000000", "' OR 1=1 --"} {
		if _, err := e.settle.CheckReturn(context.Background(), ref); !errors.Is(err, domain.ErrPaymentNotFound) {
			t.Errorf("ref %q: err = %v", ref, err)
		}
	}
}

// returnFlow checks one provider's return path against its stand-in: nothing
// changes before the provider confirms payment, and PAID follows its confirmation.
func returnFlow(t *testing.T, s *service.Settlement, p func() store.Payment, pay func()) {
	t.Helper()
	ctx := context.Background()
	id := p().ID
	v, err := s.CheckReturn(ctx, id)
	must(t, err)
	if v.Status != domain.StatusPending {
		t.Fatalf("before the provider confirms: %+v", v)
	}
	pay()
	v, err = s.CheckReturn(ctx, id)
	must(t, err)
	if v.Status != domain.StatusPaid {
		t.Fatalf("after the provider confirms: %+v", v)
	}
	if v, err = s.CheckReturn(ctx, id); err != nil || v.Status != domain.StatusPaid {
		t.Fatalf("repeat: %+v %v", v, err)
	}
}

func TestReturnXendit(t *testing.T) {
	e := newXDEnv(t)
	session, _ := e.start(t, "R-XD", "100000.00", "IDR")
	returnFlow(t, e.settle,
		func() store.Payment {
			p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "R-XD")
			return p
		},
		func() { e.srv.Complete(session, "IDR", "100000") })
}

func TestReturnMidtrans(t *testing.T) {
	e := newMTEnv(t)
	order, _ := e.start(t, "R-MT", "100000.00")
	returnFlow(t, e.settle,
		func() store.Payment {
			p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "R-MT")
			return p
		},
		func() { e.srv.Pay(order, "settlement", "", "100000.00") })
}

func TestReturnStripe(t *testing.T) {
	e := newSTEnv(t)
	session, _ := e.start(t, "R-ST", "10.00", "USD")
	returnFlow(t, e.settle,
		func() store.Payment {
			p, _ := e.payments.GetByCode(context.Background(), e.caller.ID, "R-ST")
			return p
		},
		func() { e.srv.Pay(session) })
}
