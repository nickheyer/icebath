package provider

import (
	"context"
	"time"
)

// CaptureAuxCancelCheck captures the auxiliary cancel check bound to the calling scope
func CaptureAuxCancelCheck(ctx context.Context) AuxCancelCheck { return auxCaptureAuxCancelCheck(ctx) }

// CapturedAuxCancelRequested reports whether a captured auxiliary cancel check has fired
func CapturedAuxCancelRequested(check AuxCancelCheck) bool {
	return auxCapturedAuxCancelRequested(check)
}

// CurrentAuxStreamDeadline returns the auxiliary stream deadline bound to the calling scope
func CurrentAuxStreamDeadline(ctx context.Context) (time.Time, bool) {
	return auxCurrentAuxStreamDeadline(ctx)
}

// CurrentAuxProgressHook returns the forward progress hook bound to the calling scope
func CurrentAuxProgressHook(ctx context.Context) func() error { return auxHook(ctx, AuxProgressLocal) }

// Blake2bSum is the BLAKE2b digest of data with the given output size in bytes
func Blake2bSum(data []byte, size int) []byte { return blake2bSum(data, size) }
