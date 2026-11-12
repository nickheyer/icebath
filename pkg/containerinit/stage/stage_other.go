//go:build !linux

package stage

import "context"

// Bootstrap steps run only inside the Linux image
func DispatchStageEntry(ctx context.Context) {}
