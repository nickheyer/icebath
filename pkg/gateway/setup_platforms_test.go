package gateway

import (
	"testing"
)

// Swaps a package seam for one test
func swapSetup[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}
