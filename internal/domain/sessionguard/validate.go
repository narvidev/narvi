package sessionguard

import (
	"errors"
	"fmt"
	"math"
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
// (migrations/000163) -- above zero, since a cap of zero or less permits
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
