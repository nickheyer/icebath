//go:build !(darwin && arm64)

package discord

// Variadic opus_encoder_ctl with one integer value, passed in registers like a fixed argument here
type opusCtlFunc func(st uintptr, request, value int32) int32

// Calls opus_encoder_ctl with one integer value
func callOpusCtl(fn opusCtlFunc, st uintptr, request, value int32) int32 {
	return fn(st, request, value)
}
