//go:build !linux

package gatewaycli

import "testing"

// Account namespaces exist on Linux alone, so every other system runs the case in place
func ranWhereAccountIsMapped(t *testing.T) bool { return false }
