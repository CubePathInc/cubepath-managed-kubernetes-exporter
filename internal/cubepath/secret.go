package cubepath

import (
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

// Secret is a credential that never prints itself, whatever the verb or the logger.
type Secret string

// Reveal returns the raw value. Call it only where the credential goes on the wire.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter so that every verb prints the redaction.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalText keeps the value out of JSON and text encodings.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue keeps the value out of slog output.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
