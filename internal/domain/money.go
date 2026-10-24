// Package domain holds the provider-independent payment model: money, statuses,
// the transition table and domain errors. It imports nothing from the rest of
// the application.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// currencyExponent lists currencies whose minor-unit count differs from 2.
// Any other ISO-4217 code defaults to 2.
var currencyExponent = map[string]int{
	"IDR": 0, "JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "HUF": 2,
	"KWD": 3, "BHD": 3, "OMR": 3, "JOD": 3, "TND": 3,
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	amountRe   = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,4})?$`)
)

// ErrInvalidAmount is returned for malformed, zero or over-precise amounts.
var ErrInvalidAmount = errors.New("invalid amount")

// ErrInvalidCurrency is returned for codes that are not three uppercase letters.
var ErrInvalidCurrency = errors.New("invalid currency")

// Exponent returns the number of decimal places the currency allows.
func Exponent(currency string) int {
	if e, ok := currencyExponent[currency]; ok {
		return e
	}
	return 2
}

// ValidateCurrency checks the ISO-4217 shape (not membership in a list).
func ValidateCurrency(c string) error {
	if !currencyRe.MatchString(c) {
		return fmt.Errorf("%w: %q", ErrInvalidCurrency, c)
	}
	return nil
}

// Amount is a positive decimal in a currency, held as a canonical string with
// exactly Exponent(currency) decimals. Floats are never used.
type Amount struct {
	value    string
	currency string
}

// ParseAmount validates a decimal string such as "100000.00" for a currency.
// It rejects signs, exponents, whitespace, zero and more decimals than the
// currency allows (so "10.50" is invalid for IDR, "10.5" is valid for USD).
func ParseAmount(s, currency string) (Amount, error) {
	if err := ValidateCurrency(currency); err != nil {
		return Amount{}, err
	}
	if !amountRe.MatchString(s) {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	exp := Exponent(currency)

	// Extra decimals are allowed only when they are all zeros ("100.00" for IDR).
	if len(frac) > exp {
		if strings.Trim(frac[exp:], "0") != "" {
			return Amount{}, fmt.Errorf("%w: %q has more than %d decimals for %s", ErrInvalidAmount, s, exp, currency)
		}
		frac = frac[:exp]
	}
	for len(frac) < exp {
		frac += "0"
	}

	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if strings.Trim(whole+frac, "0") == "" {
		return Amount{}, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidAmount)
	}

	v := whole
	if exp > 0 {
		v = whole + "." + frac
	}
	return Amount{value: v, currency: currency}, nil
}

// String returns the canonical decimal string.
func (a Amount) String() string { return a.value }

// Currency returns the ISO-4217 code.
func (a Amount) Currency() string { return a.currency }

// Equal reports whether two amounts are the same value in the same currency.
func (a Amount) Equal(b Amount) bool { return a.value == b.value && a.currency == b.currency }
