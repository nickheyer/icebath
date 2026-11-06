package provider

import (
	"context"
	"time"
)

// Explicit cancel source of the context for a waiter in another package, nil when none is installed
func CaptureAuxCancelCheck(ctx context.Context) AuxCancelCheck { return auxCaptureAuxCancelCheck(ctx) }

// Reads a captured cancel source, a failing source reading as not cancelled
func CapturedAuxCancelRequested(check AuxCancelCheck) bool {
	return auxCapturedAuxCancelRequested(check)
}

// The waiting host's absolute stream deadline read live, false when none is installed
func CurrentAuxStreamDeadline(ctx context.Context) (time.Time, bool) {
	return auxCurrentAuxStreamDeadline(ctx)
}

// Forward progress hook installed on the context, nil when none is
func CurrentAuxProgressHook(ctx context.Context) func() error { return auxHook(ctx, AuxProgressLocal) }

// Unkeyed BLAKE2b digest of the given size, hashlib.blake2b with digest_size
func Blake2bSum(data []byte, size int) []byte { return blake2bSum(data, size) }
