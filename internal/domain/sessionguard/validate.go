package sessionguard

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// MaxSessionSpendCapUSD is the largest cap repo_settings and automations
// can store: their session_spend_cap_usd columns are NUMERIC(10, 2).
const MaxSessionSpendCapUSD = 99_999_999.99

// ErrInvalidSessionSpendCap is ValidateSessionSpendCap's answer for a cap
// that cannot function as one.
var ErrInvalidSessionSpendCap = errors.New("sessionguard: invalid session spend cap")

// ValidateSessionSpendCap is the one check every write of a session spend
// cap makes before it writes (technical plan §37: a value that cannot
// function is refused when written). nil is valid: it clears the cap, the
// only spelling of "no cap". A cap must be a finite number -- NaN and the
// infinities are refused, as the columns' CHECK constraints refuse them
// (migrations/000165) -- above zero, since a cap of zero or less permits
// nothing, with at most two decimal places and no larger than
// MaxSessionSpendCapUSD, which is what NUMERIC(10, 2) holds exactly.
func ValidateSessionSpendCap(v *float64) error {
	if v == nil {
		return nil
	}
	f := *v
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return fmt.Errorf("%w: %v is not a number of dollars", ErrInvalidSessionSpendCap, f)
	case f <= 0:
		return fmt.Errorf("%w: %v permits no spend at all; clear the cap to remove it", ErrInvalidSessionSpendCap, f)
	case f > MaxSessionSpendCapUSD:
		return fmt.Errorf("%w: %v is above the largest cap, %.2f", ErrInvalidSessionSpendCap, f, MaxSessionSpendCapUSD)
	}
	// Two decimal places, read as the shortest decimal that round-trips the
	// float, never in exponent form -- 0.1 is "0.1" whatever its binary
	// value -- so a cap that NUMERIC(10, 2) would round is refused rather
	// than silently changed.
	if _, frac, ok := strings.Cut(strconv.FormatFloat(f, 'f', -1, 64), "."); ok && len(frac) > 2 {
		return fmt.Errorf("%w: %v has more than two decimal places", ErrInvalidSessionSpendCap, f)
	}
	return nil
}

// jsonNumber is the grammar of a JSON number (RFC 8259, section 6): an
// optional minus, an integer part with no leading zero, an optional
// fraction and an optional exponent.
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// maxCapIntegerDigits is how many digits NUMERIC(10, 2) holds before the
// decimal point.
const maxCapIntegerDigits = 8

// maxCapExponent bounds the exponent ParseSessionSpendCap reads. Any cap a
// column can hold is written with a far smaller one, and the bound keeps
// the arithmetic on the exponent in range.
const maxCapExponent = 1000

// ParseSessionSpendCap reads a session spend cap written as a JSON number
// literal -- the text of a request's capUsd, exactly as sent -- and returns
// it as a float64 that ValidateSessionSpendCap admits. It decides on the
// decimal the text spells, before any float rounding: a float64 holds about
// sixteen significant digits, so 99999999.989999999 parses to the same
// float as 99999999.99, and a check on the parsed float alone would store
// a cap the caller did not write. A literal with more than two decimal
// places -- trailing zeros aside, so 12.300 is 12.30 -- is refused, as are
// zero, a negative number, a value above MaxSessionSpendCapUSD and anything
// that is not a JSON number. An exponent is read exactly: 2.5e1 is 25 and
// 1E-2 is 0.01. Every refusal wraps ErrInvalidSessionSpendCap.
func ParseSessionSpendCap(literal string) (float64, error) {
	m := jsonNumber.FindStringSubmatch(literal)
	if m == nil {
		return 0, fmt.Errorf("%w: %q is not a number of dollars", ErrInvalidSessionSpendCap, literal)
	}
	intPart, frac := m[1], strings.TrimPrefix(m[2], ".")
	exp := 0
	if m[3] != "" {
		e, err := strconv.Atoi(m[3][1:])
		if err != nil || e > maxCapExponent || e < -maxCapExponent {
			return 0, fmt.Errorf("%w: %s is not a cap a column can hold", ErrInvalidSessionSpendCap, literal)
		}
		exp = e
	}
	// The value is digits read as an integer, times ten to the power of
	// minus scale. Leading zeros change nothing; each trailing zero dropped
	// takes one place off the scale.
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" || strings.HasPrefix(literal, "-") {
		return 0, fmt.Errorf("%w: %s permits no spend at all; clear the cap to remove it", ErrInvalidSessionSpendCap, literal)
	}
	scale := len(frac) - exp
	trimmed := strings.TrimRight(digits, "0")
	scale -= len(digits) - len(trimmed)
	digits = trimmed
	if scale > 2 {
		return 0, fmt.Errorf("%w: %s has more than two decimal places", ErrInvalidSessionSpendCap, literal)
	}
	if len(digits)-scale > maxCapIntegerDigits {
		return 0, fmt.Errorf("%w: %s is above the largest cap, %.2f", ErrInvalidSessionSpendCap, literal, MaxSessionSpendCapUSD)
	}
	// The same value as a plain decimal, which converts to the float whose
	// shortest form it is: at most ten significant digits, well inside what
	// a float64 holds.
	var text string
	if scale <= 0 {
		text = digits + strings.Repeat("0", -scale)
	} else {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		text = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrInvalidSessionSpendCap, literal, err)
	}
	if err := ValidateSessionSpendCap(&f); err != nil {
		return 0, err
	}
	return f, nil
}
