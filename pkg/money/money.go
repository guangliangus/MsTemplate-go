// Package money is the platform's wire format for monetary amounts: a JSON number carrying the
// decimal's exact digits (19.99) — never a float on the way, never a quoted string. Arithmetic
// stays on shopspring/decimal; Amount lives only at the edges, in response bodies and event
// payloads, so the wire format is a property of the type and the same in every test and every
// entry point, instead of a package-level switch only the production main flips.
package money

import (
	"github.com/shopspring/decimal"
)

// Amount is a decimal amount as it travels.
type Amount decimal.Decimal

// From wraps a decimal for the wire.
func From(d decimal.Decimal) Amount { return Amount(d) }

// Decimal unwraps the amount for arithmetic.
func (a Amount) Decimal() decimal.Decimal { return decimal.Decimal(a) }

// String is the decimal's exact representation.
func (a Amount) String() string { return decimal.Decimal(a).String() }

// MarshalJSON writes the amount as a bare JSON number.
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(decimal.Decimal(a).String()), nil
}

// UnmarshalJSON reads a JSON number, and also a numeric string (tolerant reader: a producer that
// still quotes its decimals is understood).
func (a *Amount) UnmarshalJSON(b []byte) error {
	var d decimal.Decimal
	if err := d.UnmarshalJSON(b); err != nil {
		return err
	}
	*a = Amount(d)
	return nil
}
