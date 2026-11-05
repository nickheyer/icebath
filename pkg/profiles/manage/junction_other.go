//go:build !windows

package manage

import "errors"

// Junctions exist only on Windows
func junctionTarget(string) (string, bool) { return "", false }

// Junctions exist only on Windows and are never recorded elsewhere
func createJunction(string, string) error { return errors.ErrUnsupported }
