package activesessions

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Registry rows of path, strict mode raising on a corrupt or malformed registry
func ReadEntries(path string, strict bool) ([]*jsonutil.Object, error) {
	return readEntries(path, strict)
}

// Rows whose owner is alive, strict mode requiring every owner to be provably alive
func PruneDead(entries []*jsonutil.Object, strict bool) ([]*jsonutil.Object, error) {
	return pruneDead(entries, strict, nil, nil)
}
