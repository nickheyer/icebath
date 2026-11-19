//go:build !icebath_core && !icebath_noextras

package pm

import (
	_ "embed"
)

// Index fetch behind TermuxIndex, the termux_libs index a test replaces
var termuxIndex = func() (map[string]PoolPackage, error) {
	text, err := GetText(TermuxIndexURL, nil)
	if err != nil {
		return nil, err
	}
	return ParseTermuxIndex(text), nil
}
