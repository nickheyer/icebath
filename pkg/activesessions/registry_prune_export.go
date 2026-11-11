package activesessions

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Registry rows of path, strict mode raising on a corrupt or malformed registry
func ReadEntries(path string, strict bool) ([]*pyjson.Object, error) {
	return readEntries(path, strict)
}

// Rows whose owner is alive, strict mode requiring every owner to be provably alive
func PruneDead(entries []*pyjson.Object, strict bool) ([]*pyjson.Object, error) {
	return pruneDead(entries, strict, nil, nil)
}
