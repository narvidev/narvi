package sessionguard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MicroUSD is an amount of US dollars in millionths of a dollar: the scale
// of turns.cost_usd (NUMERIC(14, 6)), so every recorded spend and every cap
// (NUMERIC(10, 2)) converts to it exactly, and two amounts compare exactly.
// A float would not: 24.999999 and 25.000000 are one micro-dollar apart, and
// the boundary between admitted and refused lies between them.
type MicroUSD int64

// microPerDollar is the number of MicroUSD in one dollar.
const microPerDollar = 1_000_000

// maxScale is the most fractional digits ParseMicroUSD accepts: a
// micro-dollar is the finest amount it can represent exactly.
const maxScale = 6

// ErrMalformedAmount is ParseMicroUSD's answer for text that is not a
// plain decimal amount of dollars with at most six fractional digits.
var ErrMalformedAmount = errors.New("sessionguard: malformed dollar amount")

// ParseMicroUSD reads s, a plain decimal amount of dollars -- digits, an
// optional fraction of at most six digits, an optional leading minus --
// as the database prints a NUMERIC, exactly: "25", "25.00", "0.000001",
// "-1.5". Anything else, a seventh fractional digit included, is
// ErrMalformedAmount rather than a rounded value, and so is an amount too
// large for an int64 of micro-dollars.
func ParseMicroUSD(s string) (MicroUSD, error) {
	text := s
	negative := false
	if strings.HasPrefix(text, "-") {
		negative = true
		text = text[1:]
	}
	whole, frac, hasPoint := strings.Cut(text, ".")
	if whole == "" || (hasPoint && frac == "") || len(frac) > maxScale || !allDigits(whole) || !allDigits(frac) {
		return 0, fmt.Errorf("%w: %q", ErrMalformedAmount, s)
	}
	frac += strings.Repeat("0", maxScale-len(frac))
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", ErrMalformedAmount, s, err)
	}
	if negative {
		v = -v
	}
	return MicroUSD(v), nil
}

// allDigits reports whether s holds ASCII digits only; "" holds none.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String prints m as dollars with a "$" sign, at least two fractional
// digits and no more than it needs: $25.00, $1.40, $24.999999, -$0.50.
func (m MicroUSD) String() string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign = "-"
		// The one value whose negation overflows prints its own digits.
		if v == -v {
			return fmt.Sprintf("-$%d.%06d", uint64(v)/microPerDollar, uint64(v)%microPerDollar)
		}
		v = -v
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", v%microPerDollar), "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return fmt.Sprintf("%s$%d.%s", sign, v/microPerDollar, frac)
}
