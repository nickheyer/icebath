package versionutil

import (
	"cmp"
	"strings"
)

// Order of two digit runs without leading zeros, by value whatever their length
func compareNumber(a, b string) int {
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

// Order of two release segments with their trailing zero parts ignored, so 1.0 and 1.0.0 are equal
func compareRelease(a, b []string) int {
	a, b = trimTrailingZeros(a), trimTrailingZeros(b)
	for i := range min(len(a), len(b)) {
		if c := compareNumber(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

func trimTrailingZeros(release []string) []string {
	end := len(release)
	for end > 0 && release[end-1] == "0" {
		end--
	}
	return release[:end]
}

// Order of the pre-release parts: a dev-release of a bare release before everything, a version without pre-release after every pre-release
func comparePre(a, b *Version) int {
	if ra, rb := preRank(a), preRank(b); ra != 0 || rb != 0 {
		return cmp.Compare(ra, rb)
	}
	if c := strings.Compare(a.preKind, b.preKind); c != 0 {
		return c
	}
	return compareNumber(a.pre, b.pre)
}

func preRank(v *Version) int {
	switch {
	case v.preKind == "" && !v.hasPost && v.hasDev:
		return -1
	case v.preKind == "":
		return 1
	}
	return 0
}

// Order of two optional numbers, an absent one sorting as missing says against a present one
func compareOptional(a string, hasA bool, b string, hasB bool, missing int) int {
	switch {
	case !hasA && !hasB:
		return 0
	case !hasA:
		return missing
	case !hasB:
		return -missing
	}
	return compareNumber(a, b)
}

// Order of two local labels: none before any, then part by part with numeric parts above words and words in byte order
func compareLocal(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return -1
	case len(b) == 0:
		return 1
	}
	for i := range min(len(a), len(b)) {
		var c int
		switch an, bn := isNumeric(a[i]), isNumeric(b[i]); {
		case an && bn:
			c = compareNumber(a[i], b[i])
		case an:
			c = 1
		case bn:
			c = -1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}
