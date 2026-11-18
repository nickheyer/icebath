//go:build !icebath_core && !icebath_noextras

package pm

import (
	_ "embed"
)

// Fetches and parses the pool's current package index
func TermuxIndex() (map[string]PoolPackage, error) { return termuxIndex() }

// Index fetch behind TermuxIndex, the termux_libs index a test replaces
var termuxIndex = func() (map[string]PoolPackage, error) {
	text, err := GetText(TermuxIndexURL, nil)
	if err != nil {
		return nil, err
	}
	return ParseTermuxIndex(text), nil
}
