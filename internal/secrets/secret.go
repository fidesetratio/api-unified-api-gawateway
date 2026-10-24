// Package secrets holds types and helpers for handling credential material.
package secrets

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

const redacted = "[REDACTED]"

// Secret wraps sensitive text so it cannot leak through formatting, JSON
// encoding or structured logging by accident. Use Reveal at the single point
// where the plaintext is really needed.
type Secret struct {
	value string
}

// New wraps a plaintext value.
func New(value string) Secret { return Secret{value: value} }

// Reveal returns the plaintext.
func (s Secret) Reveal() string { return s.value }

// IsEmpty reports whether no value is set.
func (s Secret) IsEmpty() bool { return s.value == "" }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, covering %#v.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter so every verb prints the redaction marker.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
