// Package zat converts between integer zatoshi amounts and decimal ZEC strings.
//
// All amounts inside the faucet are int64 zatoshis; decimal strings exist only at the
// edges (zecd JSON-RPC and the public API), so no floating point ever touches a value.
package zat

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PerCoin is the number of zatoshis in one ZEC (or TAZ).
const PerCoin int64 = 100_000_000

// Format renders a non-negative zatoshi amount with exactly eight decimals ("0.12500000"),
// the form zecd and Bitcoin-Core-style RPCs expect.
func Format(z int64) string {
	sign := ""
	if z < 0 {
		sign = "-"
		z = -z
	}
	return fmt.Sprintf("%s%d.%08d", sign, z/PerCoin, z%PerCoin)
}

// Display renders a zatoshi amount with trailing zeros trimmed ("0.125", "12.5", "3").
func Display(z int64) string {
	s := Format(z)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Parse converts a decimal ZEC string with at most eight fractional digits into zatoshis.
// It accepts an optional leading minus sign and rejects exponents, whitespace, and
// anything that would overflow int64.
func Parse(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty amount")
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") {
		return 0, fmt.Errorf("malformed amount %q", s)
	}
	if len(frac) > 8 {
		return 0, fmt.Errorf("amount %q has more than 8 decimal places", s)
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("malformed amount %q", s)
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q out of range", s)
	}
	f := int64(0)
	if frac != "" {
		f, err = strconv.ParseInt(frac+strings.Repeat("0", 8-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed amount %q", s)
		}
	}
	if w > (1<<63-1-f)/PerCoin {
		return 0, fmt.Errorf("amount %q out of range", s)
	}
	z := w*PerCoin + f
	if neg {
		z = -z
	}
	return z, nil
}
