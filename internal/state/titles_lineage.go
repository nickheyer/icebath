package state

import (
	"context"
	"math/big"
	"regexp"
)

// Title with a numbered continuation suffix, the base and the number
var numberedTitle = regexp.MustCompile(`^(.*?) #(\d+)$`)

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
		if n, ok := new(big.Int).SetString(m[2], 10); ok && n.Cmp(highest) > 0 {
			highest = n
		}
	}
	return base + " #" + new(big.Int).Add(highest, big.NewInt(1)).String(), nil
}
