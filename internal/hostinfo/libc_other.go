//go:build !linux

package hostinfo

import (
	"context"
	"fmt"
)

// The Linux userland C library exists only on Linux
func probeLibC(context.Context) (LibC, error) {
	return LibC{}, fmt.Errorf("linux C library: %w", ErrAbsent)
}
