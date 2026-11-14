package state

import (
	"context"
	"math/big"
	"regexp"
	"unicode"
)

// Title with a numbered continuation suffix, the base and the number in any decimal digits
var numberedTitle = regexp.MustCompile(`^(.*?) #(\p{Nd}+)$`)

// Number a run of decimal digits of any script spells, false when a rune is no decimal digit
func decimalNumber(digits string) (*big.Int, bool) {
	n, ten := new(big.Int), big.NewInt(10)
	for _, r := range digits {
		value, ok := decimalValue(r)
		if !ok {
			return nil, false
		}
		n.Mul(n, ten).Add(n, big.NewInt(value))
	}
	return n, true
}

// Value of one decimal digit, every script laying its digits out as runs of zero to nine
func decimalValue(r rune) (int64, bool) {
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int64(r-rune(rg.Lo)) % 10, true
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int64(r-rune(rg.Lo)) % 10, true
		}
	}
	return 0, false
}

// Next title in a lineage as c sees it, any number suffix stripped and the highest existing number incremented
func nextTitleInLineage(ctx context.Context, c Conn, baseTitle string) (string, error) {
	base := baseTitle
	if m := numberedTitle.FindStringSubmatch(baseTitle); m != nil {
		base = m[1]
	}
	titles, err := queryStrings(ctx, c, `SELECT title FROM sessions WHERE title = ? OR title LIKE ? ESCAPE '\'`, base, EscapeLike(base)+" #%")
	if err != nil {
		return "", err
	}
	if len(titles) == 0 {
		return base, nil
	}
	highest := big.NewInt(1)
	for _, t := range titles {
		m := numberedTitle.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		if n, ok := decimalNumber(m[2]); ok && n.Cmp(highest) > 0 {
			highest = n
		}
	}
	return base + " #" + new(big.Int).Add(highest, big.NewInt(1)).String(), nil
}
