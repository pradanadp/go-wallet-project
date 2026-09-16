package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Amount is a monetary value in minor units (integer).
// Example: Rp10.000,00 -> Amount(1_000_000) with scale 2.
//
// Integer instead of float64 because binary floating point cannot represent
// decimal money exactly (0.1 + 0.2 != 0.3) and the error would accumulate in the
// balance. The API also takes and returns minor units directly, so no conversion
// can lose precision.
type Amount int64

// MaxAmount is the upper bound per transaction (1e15 minor units), which keeps
// int64 accumulation from overflowing.
const MaxAmount Amount = 1_000_000_000_000_000

// NewAmount validates an amount coming from external input.
func NewAmount(field string, minor int64) (Amount, error) {
	if minor <= 0 {
		return 0, NewValidationError(field, "must be greater than 0")
	}
	if Amount(minor) > MaxAmount {
		return 0, NewValidationError(field, fmt.Sprintf("exceeds the maximum of %d minor units", int64(MaxAmount)))
	}
	return Amount(minor), nil
}

func (a Amount) Int64() int64 { return int64(a) }

func (a Amount) String() string { return strconv.FormatInt(int64(a), 10) }

// Currency is an ISO-4217 currency code (three uppercase letters).
type Currency string

const CurrencyIDR Currency = "IDR"

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func ParseCurrency(field, raw string) (Currency, error) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	if !currencyPattern.MatchString(v) {
		return "", NewValidationError(field, "must be a three-letter uppercase ISO-4217 code (e.g. IDR)")
	}
	return Currency(v), nil
}

func (c Currency) String() string { return string(c) }
