package domain

import (
	"errors"
	"testing"
)

func TestParseAmount(t *testing.T) {
	tests := []struct {
		name, in, cur string
		want          string
		wantErr       bool
	}{
		{"usd two decimals", "10.00", "USD", "10.00", false},
		{"usd pads", "10.5", "USD", "10.50", false},
		{"usd integer", "10", "USD", "10.00", false},
		{"idr sample", "100000.00", "IDR", "100000", false},
		{"idr integer", "100000", "IDR", "100000", false},
		{"idr real fraction", "100000.50", "IDR", "", true},
		{"usd three decimals", "10.123", "USD", "", true},
		{"usd extra zero ok", "10.000", "USD", "10.00", false},
		{"kwd three decimals", "1.234", "KWD", "1.234", false},
		{"leading zeros", "007.10", "USD", "7.10", false},
		{"zero", "0", "USD", "", true},
		{"zero decimal", "0.00", "USD", "", true},
		{"negative", "-1.00", "USD", "", true},
		{"plus sign", "+1.00", "USD", "", true},
		{"exponent", "1e3", "USD", "", true},
		{"empty", "", "USD", "", true},
		{"spaces", " 1.00", "USD", "", true},
		{"comma", "1,00", "USD", "", true},
		{"too long", "12345678901234567.00", "USD", "", true},
		{"lowercase currency", "1.00", "usd", "", true},
		{"bad currency", "1.00", "US", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAmount(tt.in, tt.cur)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tt.want || got.Currency() != tt.cur {
				t.Errorf("got %q %s, want %q", got, got.Currency(), tt.want)
			}
		})
	}
}

func TestAmountErrorsWrapSentinels(t *testing.T) {
	_, err := ParseAmount("x", "USD")
	if !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("got %v", err)
	}
	_, err = ParseAmount("1", "us")
	if !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("got %v", err)
	}
}

func TestAmountEqualIgnoresFormatting(t *testing.T) {
	a, _ := ParseAmount("10", "USD")
	b, _ := ParseAmount("10.00", "USD")
	c, _ := ParseAmount("10.00", "EUR")
	if !a.Equal(b) {
		t.Error("10 and 10.00 should be equal")
	}
	if a.Equal(c) {
		t.Error("different currencies must differ")
	}
}

// TestTransitionTable checks every ordered pair of statuses.
func TestTransitionTable(t *testing.T) {
	allowed := map[[2]Status]Outcome{
		{StatusCreating, StatusPending}:   OutcomeAllowed,
		{StatusCreating, StatusFailed}:    OutcomeAllowed,
		{StatusCreating, StatusExpired}:   OutcomeAllowed,
		{StatusCreating, StatusCancelled}: OutcomeAllowed,
		{StatusCreating, StatusPaid}:      OutcomeAllowed,
		{StatusPending, StatusPaid}:       OutcomeAllowed,
		{StatusPending, StatusFailed}:     OutcomeAllowed,
		{StatusPending, StatusExpired}:    OutcomeAllowed,
		{StatusPending, StatusCancelled}:  OutcomeAllowed,
		{StatusPaid, StatusRefunded}:      OutcomeAllowed,
		{StatusExpired, StatusPaid}:       OutcomeAllowedNeedsReview,
		{StatusCancelled, StatusPaid}:     OutcomeAllowedNeedsReview,
	}
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			want := OutcomeRejected
			if from == to {
				want = OutcomeNoop
			} else if o, ok := allowed[[2]Status{from, to}]; ok {
				want = o
			}
			if got := CheckTransition(from, to); got != want {
				t.Errorf("%s -> %s: got %d want %d", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesStayTerminal(t *testing.T) {
	for _, from := range []Status{StatusFailed, StatusRefunded} {
		for _, to := range AllStatuses {
			if to != from && CheckTransition(from, to) != OutcomeRejected {
				t.Errorf("%s -> %s should be rejected", from, to)
			}
		}
	}
}

func TestUnknownStatusRejected(t *testing.T) {
	if CheckTransition("bogus", StatusPaid) != OutcomeRejected || CheckTransition(StatusPending, "bogus") != OutcomeRejected {
		t.Error("unknown statuses must be rejected")
	}
}

func TestAPIName(t *testing.T) {
	want := map[Status]string{
		StatusCreating: "PENDING", StatusPending: "PENDING", StatusPaid: "PAID",
		StatusFailed: "FAILED", StatusExpired: "EXPIRED", StatusCancelled: "CANCELLED",
		StatusRefunded: "REFUNDED",
	}
	for s, w := range want {
		if s.APIName() != w {
			t.Errorf("%s -> %s want %s", s, s.APIName(), w)
		}
	}
}
