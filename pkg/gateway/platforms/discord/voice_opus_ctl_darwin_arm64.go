package discord

// Variadic opus_encoder_ctl, the Apple arm64 ABI reading the variadic value from the first stack slot
type opusCtlFunc func(st uintptr, request int32, r2, r3, r4, r5, r6, r7, value uintptr) int32

// Calls opus_encoder_ctl with one integer value placed past the eight argument registers
func callOpusCtl(fn opusCtlFunc, st uintptr, request, value int32) int32 {
	return fn(st, request, 0, 0, 0, 0, 0, 0, uintptr(int64(value)))
}
