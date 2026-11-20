//go:build linux && (mips || mipsle || mips64 || mips64le)

package tty

// Bytes of the kernel signal set, 128 signals on MIPS
const sigsetSize = 16
